package flow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/db"
	messagepkg "github.com/memohai/memoh/internal/message"
	pipelinepkg "github.com/memohai/memoh/internal/pipeline"
)

type messageWithUsage struct {
	ID                string
	Message           conversation.ModelMessage
	UsageInputTokens  *int
	UsageOutputTokens *int
	SessionID         string
	ExternalMessageID string
	Platform          string
	SenderChannelID   string
	CompactID         string
	Required          bool
}

func (r *Resolver) loadMessages(ctx context.Context, chatID string, sessionID string, maxContextMinutes int) ([]messageWithUsage, error) {
	if r.messageService == nil {
		return nil, nil
	}
	since := time.Now().UTC().Add(-time.Duration(maxContextMinutes) * time.Minute)
	var msgs []messagepkg.Message
	var err error
	if strings.TrimSpace(sessionID) != "" {
		msgs, err = r.messageService.ListActiveSinceBySession(ctx, sessionID, since)
	} else {
		msgs, err = r.messageService.ListActiveSince(ctx, chatID, since)
	}
	if err != nil {
		return nil, err
	}
	var result []messageWithUsage
	for _, m := range msgs {
		result = append(result, r.messageWithUsageFromPersisted(chatID, m))
	}
	return result, nil
}

func (r *Resolver) messageWithUsageFromPersisted(chatID string, m messagepkg.Message) messageWithUsage {
	var mm conversation.ModelMessage
	if err := json.Unmarshal(m.Content, &mm); err != nil {
		r.logger.Warn("loadMessages: content unmarshal failed, treating as raw text",
			slog.String("chat_id", chatID), slog.Any("error", err))
		mm = conversation.ModelMessage{Role: m.Role, Content: m.Content}
	} else {
		mm.Role = m.Role
	}
	var inputTokens *int
	var outputTokens *int
	if len(m.Usage) > 0 {
		var u usageInfo
		if json.Unmarshal(m.Usage, &u) == nil {
			inputTokens = u.InputTokens
			outputTokens = u.OutputTokens
		}
	}
	return messageWithUsage{
		ID:                strings.TrimSpace(m.ID),
		Message:           mm,
		UsageInputTokens:  inputTokens,
		UsageOutputTokens: outputTokens,
		SessionID:         strings.TrimSpace(m.SessionID),
		ExternalMessageID: strings.TrimSpace(m.ExternalMessageID),
		Platform:          strings.TrimSpace(m.Platform),
		SenderChannelID:   strings.TrimSpace(m.SenderChannelIdentityID),
		CompactID:         strings.TrimSpace(m.CompactID),
	}
}

func (r *Resolver) ensureRequiredHistoryMessage(ctx context.Context, messages []messageWithUsage, req conversation.ChatRequest) ([]messageWithUsage, error) {
	messageID := strings.TrimSpace(req.RequiredHistoryMessageID)
	if messageID == "" || r.messageService == nil || strings.TrimSpace(req.SessionID) == "" {
		return messages, nil
	}
	for i, item := range messages {
		if strings.TrimSpace(item.ID) == messageID {
			messages[i].Required = true
			return messages, nil
		}
	}
	window, err := r.messageService.ListVisibleFromBySession(ctx, req.SessionID, messageID)
	if err != nil {
		return nil, err
	}
	required := make([]messageWithUsage, 0, len(window))
	for _, msg := range window {
		required = append(required, r.messageWithUsageFromPersisted(req.ChatID, msg))
	}
	required = pruneHistoryForGateway(required)
	required = filterMessagesBeforeID(required, req.HistoryCutoffBeforeMessageID)
	if !containsMessageWithUsage(required, messageID) {
		return nil, errors.New("required history message is not visible")
	}
	for i := range required {
		if strings.TrimSpace(required[i].ID) == messageID {
			required[i].Required = true
		}
	}
	return mergeRequiredHistoryWindow(messages, required), nil
}

func containsMessageWithUsage(messages []messageWithUsage, messageID string) bool {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false
	}
	for _, item := range messages {
		if strings.TrimSpace(item.ID) == messageID {
			return true
		}
	}
	return false
}

func mergeRequiredHistoryWindow(messages []messageWithUsage, required []messageWithUsage) []messageWithUsage {
	if len(required) == 0 {
		return messages
	}
	requiredIDs := make(map[string]struct{}, len(required))
	for _, item := range required {
		if id := strings.TrimSpace(item.ID); id != "" {
			requiredIDs[id] = struct{}{}
		}
	}
	merged := make([]messageWithUsage, 0, len(messages)+len(required))
	for _, item := range messages {
		if _, ok := requiredIDs[strings.TrimSpace(item.ID)]; ok {
			continue
		}
		merged = append(merged, item)
	}
	return append(merged, required...)
}

func filterMessagesBeforeID(messages []messageWithUsage, messageID string) []messageWithUsage {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return messages
	}
	for i, item := range messages {
		if strings.TrimSpace(item.ID) == messageID {
			return messages[:i]
		}
	}
	return messages
}

func dedupePersistedCurrentUserMessage(messages []messageWithUsage, req conversation.ChatRequest) []messageWithUsage {
	if !req.UserMessagePersisted || len(messages) == 0 {
		return messages
	}

	targetSessionID := strings.TrimSpace(req.SessionID)
	targetExternalID := strings.TrimSpace(req.ExternalMessageID)
	targetPlatform := strings.TrimSpace(req.CurrentChannel)
	targetSenderChannelID := strings.TrimSpace(req.SourceChannelIdentityID)
	if targetExternalID == "" {
		return messages
	}

	for i := len(messages) - 1; i >= 0; i-- {
		item := messages[i]
		if !strings.EqualFold(strings.TrimSpace(item.Message.Role), "user") {
			continue
		}
		if strings.TrimSpace(item.ExternalMessageID) != targetExternalID {
			continue
		}
		if targetSessionID != "" && item.SessionID != "" && item.SessionID != targetSessionID {
			continue
		}
		if targetPlatform != "" && item.Platform != "" && !strings.EqualFold(item.Platform, targetPlatform) {
			continue
		}
		if targetSenderChannelID != "" && item.SenderChannelID != "" && item.SenderChannelID != targetSenderChannelID {
			continue
		}
		return append(messages[:i], messages[i+1:]...)
	}

	return messages
}

func estimateMessageTokens(msg conversation.ModelMessage) int {
	text := msg.TextContent()
	if len(text) == 0 {
		data, _ := json.Marshal(msg.Content)
		return len(data) / 4
	}
	return len(text) / 4
}

func modelMessagesOf(messages []messageWithUsage) []conversation.ModelMessage {
	result := make([]conversation.ModelMessage, len(messages))
	for i, m := range messages {
		result[i] = m.Message
	}
	return result
}

func estimateModelMessagesTokens(messages []conversation.ModelMessage) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m)
	}
	return total
}

func (r *Resolver) replaceCompactedMessages(ctx context.Context, messages []messageWithUsage) []messageWithUsage {
	if r.queries == nil {
		return messages
	}

	compactGroups := make(map[string][]int) // compact_id -> indices
	requiredCompactGroups := make(map[string]bool)
	for i, m := range messages {
		if m.CompactID != "" {
			compactGroups[m.CompactID] = append(compactGroups[m.CompactID], i)
			if m.Required {
				requiredCompactGroups[m.CompactID] = true
			}
		}
	}
	if len(compactGroups) == 0 {
		return messages
	}

	summaries := make(map[string]string)
	for compactID := range compactGroups {
		cUUID, err := db.ParseUUID(compactID)
		if err != nil {
			continue
		}
		log, err := r.queries.GetCompactionLogByID(ctx, cUUID)
		if err != nil {
			r.logger.Warn("replaceCompactedMessages: failed to load compact log", slog.String("compact_id", compactID), slog.Any("error", err))
			continue
		}
		if log.Status == "ok" && log.Summary != "" {
			summaries[compactID] = log.Summary
		}
	}

	var result []messageWithUsage
	replaced := make(map[string]bool)
	for _, m := range messages {
		if m.CompactID == "" {
			result = append(result, m)
			continue
		}
		if replaced[m.CompactID] {
			continue
		}
		replaced[m.CompactID] = true
		if requiredCompactGroups[m.CompactID] {
			for _, idx := range compactGroups[m.CompactID] {
				result = append(result, messages[idx])
			}
			continue
		}
		summary, ok := summaries[m.CompactID]
		if !ok || summary == "" {
			for _, idx := range compactGroups[m.CompactID] {
				result = append(result, messages[idx])
			}
			continue
		}
		result = append(result, messageWithUsage{
			Message: conversation.ModelMessage{
				Role:    "user",
				Content: json.RawMessage(`"<summary>\n` + summary + `\n</summary>"`),
			},
		})
	}
	return result
}

// buildMessagesFromPipeline assembles chat context from the DCP pipeline's
// RenderedContext (RC) merged with assistant/tool turns (TR) from
// bot_history_messages. This gives chat mode the same event-driven context
// that discuss mode uses, replacing the legacy loadMessages path.
func (r *Resolver) buildMessagesFromPipeline(ctx context.Context, req conversation.ChatRequest, contextTokenBudget int) []conversation.ModelMessage {
	sessionID := strings.TrimSpace(req.SessionID)
	if r.pipeline == nil || sessionID == "" {
		return nil
	}
	rc := r.pipeline.GetRC(sessionID)
	if len(rc) == 0 {
		return nil
	}

	trs := r.loadTurnResponses(ctx, sessionID)

	composed := pipelinepkg.ComposeContext(rc, trs, "")
	if composed == nil {
		return nil
	}

	messages := make([]conversation.ModelMessage, 0, len(composed.Messages))
	for _, m := range composed.Messages {
		contentJSON := m.RawContent
		if len(contentJSON) == 0 {
			var err error
			contentJSON, err = json.Marshal(m.Content)
			if err != nil {
				continue
			}
		}
		messages = append(messages, conversation.ModelMessage{
			Role:    m.Role,
			Content: contentJSON,
		})
	}

	// Apply context token budget trimming to pipeline path as well.
	if contextTokenBudget > 0 && len(messages) > 0 {
		messages = trimPipelineMessagesByTokens(r.logger, messages, contextTokenBudget)
	}

	return messages
}

// trimPipelineMessagesByTokens trims pipeline-assembled messages to fit within
// the context token budget using character-based estimation.
func trimPipelineMessagesByTokens(log *slog.Logger, messages []conversation.ModelMessage, maxTokens int) []conversation.ModelMessage {
	totalTokens := 0
	cutoff := 0
	for i := len(messages) - 1; i >= 0; i-- {
		totalTokens += estimateMessageTokens(messages[i])
		if totalTokens > maxTokens {
			cutoff = i + 1
			break
		}
	}

	// Avoid orphaned tool messages at the cutoff boundary.
	for cutoff < len(messages) && strings.EqualFold(strings.TrimSpace(messages[cutoff].Role), "tool") {
		cutoff++
	}

	if cutoff > 0 && log != nil {
		log.Info("trimPipelineMessagesByTokens: context trimmed",
			slog.Int("total_messages", len(messages)),
			slog.Int("estimated_tokens", totalTokens),
			slog.Int("max_tokens", maxTokens),
			slog.Int("kept_messages", len(messages)-cutoff),
		)
	}

	return messages[cutoff:]
}

// loadTurnResponses loads recent assistant/tool messages from bot_history_messages
// for use as the TR stream in pipeline-based context assembly.
func (r *Resolver) loadTurnResponses(ctx context.Context, sessionID string) []pipelinepkg.TurnResponseEntry {
	if r.messageService == nil {
		return nil
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	msgs, err := r.messageService.ListActiveSinceBySession(ctx, sessionID, since)
	if err != nil {
		r.logger.Warn("load TRs failed", slog.String("session_id", sessionID), slog.Any("error", err))
		return nil
	}
	var trs []pipelinepkg.TurnResponseEntry
	for _, m := range msgs {
		entry, ok := pipelinepkg.DecodeTurnResponseEntry(m)
		if !ok {
			continue
		}
		trs = append(trs, entry)
	}
	return trs
}
