package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
	messageevent "github.com/felinics/memoh/internal/chat/event"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func (w *Writer) ReplaceTurn(
	ctx context.Context,
	req runreq.ChatRequest,
	oldTurnID string,
	requestMessageID string,
	reason string,
	persisted []messagepkg.Message,
) error {
	replacementID := firstAssistantID(persisted)
	if replacementID == "" {
		replacementID = latestPersistedID(persisted)
	}
	if replacementID == "" {
		return errors.New("replacement message was not persisted")
	}
	if strings.TrimSpace(requestMessageID) == "" {
		requestMessageID = firstUserID(persisted)
	}
	forkAnchorUpdate := w.PrepareForkAnchorUpdate(ctx, req.ThreadID, req.HistoryCutoffBeforeMessageID)
	if _, err := w.messages.ReplaceTurn(
		context.WithoutCancel(ctx),
		req.ThreadID,
		oldTurnID,
		req.TurnID,
		req.TurnPosition,
		requestMessageID,
		replacementID,
		reason,
	); err != nil {
		w.logger.ErrorContext(ctx, "replace history turn failed", slog.String("reason", reason), slog.Any("error", err))
		w.CleanupReplacementMessages(ctx, persisted)
		return fmt.Errorf("replace history turn: %w", err)
	}
	w.applyForkAnchorUpdate(context.WithoutCancel(ctx), req.ThreadID, forkAnchorUpdate)
	w.publishReplacementMessageCreated(req.BotID, persisted)
	return nil
}

func (w *Writer) publishReplacementMessageCreated(botID string, persisted []messagepkg.Message) {
	if len(persisted) == 0 {
		return
	}
	var latest messagepkg.Message
	for i := len(persisted) - 1; i >= 0; i-- {
		if strings.TrimSpace(persisted[i].ID) == "" {
			continue
		}
		latest = persisted[i]
		break
	}
	if strings.TrimSpace(latest.ID) == "" {
		return
	}
	eventBotID := strings.TrimSpace(botID)
	if eventBotID == "" {
		eventBotID = strings.TrimSpace(latest.BotID)
	}
	data, err := json.Marshal(latest)
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("marshal replacement message event failed", slog.Any("error", err))
		}
		return
	}
	w.events.Publish(messageevent.Event{
		Type:  messageevent.EventTypeMessageCreated,
		BotID: eventBotID,
		Data:  data,
	})
}

type ForkAnchorUpdate struct {
	Metadata map[string]any
}

func (w *Writer) PrepareForkAnchorUpdate(ctx context.Context, sessionID string, replacedTailStartMessageID string) *ForkAnchorUpdate {
	sessionID = strings.TrimSpace(sessionID)
	replacedTailStartMessageID = strings.TrimSpace(replacedTailStartMessageID)
	if w.sessions == nil || w.messages == nil || sessionID == "" || replacedTailStartMessageID == "" {
		return nil
	}

	sess, err := w.sessions.Get(ctx, sessionID)
	if err != nil {
		w.logForkAnchorUpdateWarning("load session for fork anchor update failed", sessionID, err)
		return nil
	}
	fork, ok := forkedFromMetadata(sess.Metadata)
	if !ok {
		return nil
	}
	currentAnchor := forkMetadataString(fork, "fork_message_id")
	if currentAnchor == "" {
		return nil
	}

	replacedTail, err := w.messages.ListVisibleFromBySession(ctx, sessionID, replacedTailStartMessageID)
	if err != nil {
		w.logForkAnchorUpdateWarning("load replaced tail for fork anchor update failed", sessionID, err)
		return nil
	}
	if !messagesContainAnchor(replacedTail, currentAnchor) {
		return nil
	}

	nextAnchor := w.latestInheritedAssistantBefore(ctx, sessionID, replacedTailStartMessageID)
	if nextAnchor == currentAnchor {
		return nil
	}

	nextFork := runreq.CloneMetadataMap(fork)
	if nextAnchor != "" {
		nextFork["fork_message_id"] = nextAnchor
	} else {
		delete(nextFork, "fork_message_id")
	}

	nextMetadata := runreq.CloneMetadataMap(sess.Metadata)
	nextMetadata["forked_from"] = nextFork
	return &ForkAnchorUpdate{Metadata: nextMetadata}
}

func (w *Writer) applyForkAnchorUpdate(ctx context.Context, sessionID string, update *ForkAnchorUpdate) {
	sessionID = strings.TrimSpace(sessionID)
	if w.sessions == nil || update == nil || sessionID == "" {
		return
	}
	if _, err := w.sessions.UpdateMetadata(ctx, sessionID, update.Metadata); err != nil {
		w.logForkAnchorUpdateWarning("persist fork anchor update failed", sessionID, err)
	}
}

func (w *Writer) latestInheritedAssistantBefore(ctx context.Context, sessionID string, beforeMessageID string) string {
	messages, err := w.messages.ListBeforeMessageBySession(ctx, sessionID, beforeMessageID, 10000)
	if err != nil {
		w.logForkAnchorUpdateWarning("load previous messages for fork anchor update failed", sessionID, err)
		return ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			continue
		}
		return strings.TrimSpace(msg.ID)
	}
	return ""
}

func messagesContainAnchor(messages []messagepkg.Message, anchor string) bool {
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return false
	}
	for _, msg := range messages {
		if strings.TrimSpace(msg.ID) == anchor || strings.TrimSpace(msg.ExternalMessageID) == anchor {
			return true
		}
	}
	return false
}

func forkedFromMetadata(metadata map[string]any) (map[string]any, bool) {
	raw, ok := metadata["forked_from"]
	if !ok || raw == nil {
		return nil, false
	}
	switch value := raw.(type) {
	case map[string]any:
		return value, true
	case map[string]string:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = item
		}
		return out, true
	default:
		return nil, false
	}
}

func forkMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func (w *Writer) logForkAnchorUpdateWarning(message string, sessionID string, err error) {
	if w.logger != nil {
		w.logger.Warn("fork anchor update skipped", slog.String("reason", message), slog.String("session_id", sessionID), slog.Any("error", err))
	}
}

func (w *Writer) CleanupReplacementMessages(ctx context.Context, persisted []messagepkg.Message) {
	if w.messages == nil || len(persisted) == 0 {
		return
	}
	ids := make([]string, 0, len(persisted))
	for _, msg := range persisted {
		if id := strings.TrimSpace(msg.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := w.messages.DeleteByIDs(context.WithoutCancel(ctx), ids); err != nil {
		w.logger.ErrorContext(ctx, "cleanup replacement messages failed", slog.Any("error", err), slog.Int("message_count", len(ids)))
	}
}

func VisibleMessageText(msg messagepkg.Message) string {
	if text := strings.TrimSpace(msg.DisplayContent); text != "" {
		return text
	}
	var modelMessage runreq.ModelMessage
	if err := json.Unmarshal(msg.Content, &modelMessage); err == nil {
		if text := strings.TrimSpace(modelMessage.TextContent()); text != "" {
			return text
		}
	}
	return strings.TrimSpace(string(msg.Content))
}

func firstAssistantID(messages []messagepkg.Message) string {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			return strings.TrimSpace(msg.ID)
		}
	}
	return ""
}

func firstUserID(messages []messagepkg.Message) string {
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			return strings.TrimSpace(msg.ID)
		}
	}
	return ""
}

func latestPersistedID(messages []messagepkg.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if id := strings.TrimSpace(messages[i].ID); id != "" {
			return id
		}
	}
	return ""
}
