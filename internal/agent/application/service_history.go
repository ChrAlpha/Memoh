package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
)

func injectWorkspaceTransitionRecords(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(records) == 0 {
		return records
	}
	result := make([]historyfrag.HistoryRecord, 0, len(records)+2)
	var previous *WorkspaceTarget
	for _, record := range records {
		if current := workspaceTargetFromMetadata(record.Metadata); current != nil && !sameWorkspaceTarget(previous, current) {
			text := renderWorkspaceTransition(previous, current)
			result = append(result, historyfrag.HistoryRecord{
				ModelMessage: ModelMessage{Role: "system", Content: newTextContent(text)},
				Synthetic:    true,
			})
			previous = current
		}
		result = append(result, record)
	}
	return result
}

func renderWorkspaceTransition(previous, current *WorkspaceTarget) string {
	if current == nil || strings.TrimSpace(current.TargetID) == "" {
		return ""
	}
	if previous != nil && sameWorkspaceTarget(previous, current) {
		return ""
	}
	if previous == nil {
		return fmt.Sprintf(
			"[Execution location] The default Computer is %q (target_id=%q, kind=%q). Workspace tools that omit target_id run there.",
			current.Name,
			current.TargetID,
			current.Kind,
		)
	}
	return fmt.Sprintf(
		"[Execution location changed] The default Computer changed from %q (target_id=%q, kind=%q) to %q (target_id=%q, kind=%q). Earlier file and command results belong to their recorded Computer. Files, processes, and working-directory state do not transfer between Computers; inspect the new Computer before continuing.",
		previous.Name,
		previous.TargetID,
		previous.Kind,
		current.Name,
		current.TargetID,
		current.Kind,
	)
}

func sameWorkspaceTarget(left, right *WorkspaceTarget) bool {
	if left == nil || right == nil {
		return left == right
	}
	return strings.TrimSpace(left.TargetID) == strings.TrimSpace(right.TargetID) &&
		strings.TrimSpace(left.Kind) == strings.TrimSpace(right.Kind)
}

func workspaceTargetFromMetadata(metadata map[string]any) *WorkspaceTarget {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["execution_location"].(map[string]any)
	if !ok {
		return nil
	}
	target := &WorkspaceTarget{
		TargetID: strings.TrimSpace(readAnyString(raw["target_id"])),
		Kind:     strings.TrimSpace(readAnyString(raw["kind"])),
		Name:     strings.TrimSpace(readAnyString(raw["name"])),
	}
	if target.TargetID == "" {
		return nil
	}
	if target.Name == "" {
		target.Name = target.TargetID
	}
	return target
}

func (s *Service) currentWorkspaceContextMessage(ctx context.Context, req ChatRequest) *ModelMessage {
	current := req.WorkspaceTarget
	if current == nil || strings.TrimSpace(current.TargetID) == "" {
		return nil
	}
	var previous *WorkspaceTarget
	if s != nil && s.sessionService != nil && strings.TrimSpace(req.ThreadID) != "" {
		if sess, err := s.sessionService.Get(ctx, req.ThreadID); err == nil {
			if raw, ok := sess.Metadata["workspace_target"].(map[string]any); ok {
				previous = workspaceTargetFromMetadata(map[string]any{"execution_location": raw})
			}
		}
	}
	text := renderWorkspaceTransition(previous, current)
	if text == "" {
		return nil
	}
	message := ModelMessage{Role: "system", Content: newTextContent(text)}
	return &message
}

func historyScopeFallbackFromChatRequest(req ChatRequest) historyfrag.ScopeFallback {
	return historyfrag.ScopeFallback{
		ChatID:           strings.TrimSpace(req.ChatID),
		ConversationType: strings.TrimSpace(req.ConversationType),
		ConversationName: strings.TrimSpace(req.ConversationName),
		ReplyTarget:      strings.TrimSpace(req.ReplyTarget),
	}
}

func historyScopeFallbackFromUserInputRequest(req userinput.Request) historyfrag.ScopeFallback {
	return historyfrag.ScopeFallback{
		ConversationType: strings.TrimSpace(req.ConversationType),
		ReplyTarget:      strings.TrimSpace(req.ReplyTarget),
	}
}

func historyScopeFallbackFromToolApprovalRequest(req toolapproval.Request) historyfrag.ScopeFallback {
	return historyfrag.ScopeFallback{
		ConversationType: strings.TrimSpace(req.ConversationType),
		ReplyTarget:      strings.TrimSpace(req.ReplyTarget),
	}
}

func (s *Service) ensureRequiredHistoryMessage(ctx context.Context, messages []historyfrag.HistoryRecord, req ChatRequest) ([]historyfrag.HistoryRecord, error) {
	messageID := strings.TrimSpace(req.RequiredHistoryMessageID)
	if messageID == "" || s.messageService == nil || strings.TrimSpace(req.ThreadID) == "" {
		return messages, nil
	}
	for i, item := range messages {
		if strings.TrimSpace(item.DBMessageID) == messageID {
			messages[i].Required = true
			return messages, nil
		}
	}
	window, err := s.messageService.ListVisibleFromBySession(ctx, req.ThreadID, messageID)
	if err != nil {
		return nil, err
	}
	fallback := historyScopeFallbackFromChatRequest(req)
	required := make([]historyfrag.HistoryRecord, 0, len(window))
	for _, msg := range window {
		record, err := historyfrag.FromDBMessage(msg, fallback)
		if err != nil {
			return nil, err
		}
		required = append(required, record)
	}
	required = filterMessagesBeforeID(required, req.HistoryCutoffBeforeMessageID)
	if !containsHistoryRecord(required, messageID) {
		return nil, errors.New("required history message is not visible")
	}
	for i := range required {
		if strings.TrimSpace(required[i].DBMessageID) == messageID {
			required[i].Required = true
		}
	}
	return mergeRequiredHistoryWindow(messages, required), nil
}

func containsHistoryRecord(messages []historyfrag.HistoryRecord, messageID string) bool {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false
	}
	for _, item := range messages {
		if strings.TrimSpace(item.DBMessageID) == messageID {
			return true
		}
	}
	return false
}

func mergeRequiredHistoryWindow(messages []historyfrag.HistoryRecord, required []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(required) == 0 {
		return messages
	}
	requiredIDs := make(map[string]struct{}, len(required))
	for _, item := range required {
		if id := strings.TrimSpace(item.DBMessageID); id != "" {
			requiredIDs[id] = struct{}{}
		}
	}
	merged := make([]historyfrag.HistoryRecord, 0, len(messages)+len(required))
	for _, item := range messages {
		if _, ok := requiredIDs[strings.TrimSpace(item.DBMessageID)]; ok {
			continue
		}
		merged = append(merged, item)
	}
	return append(merged, required...)
}

func filterMessagesBeforeID(messages []historyfrag.HistoryRecord, messageID string) []historyfrag.HistoryRecord {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return messages
	}
	for i, item := range messages {
		if strings.TrimSpace(item.DBMessageID) == messageID {
			return messages[:i]
		}
	}
	return nil
}

func compactionSummaryScope(botID, chatID, sessionID, conversationType, conversationName, replyTarget string) contextfrag.Scope {
	return contextfrag.Scope{
		BotID:            strings.TrimSpace(botID),
		ChatID:           strings.TrimSpace(chatID),
		SessionID:        strings.TrimSpace(sessionID),
		ConversationType: strings.TrimSpace(conversationType),
		ConversationName: strings.TrimSpace(conversationName),
		ReplyTarget:      strings.TrimSpace(replyTarget),
	}
}

// dedupePersistedCurrentUserMessage removes the current user row from history
// when the request already persisted it. The row id is authoritative; the
// external message id only identifies rows of requests that carry no id.
func dedupePersistedCurrentUserMessage(messages []historyfrag.HistoryRecord, req ChatRequest) []historyfrag.HistoryRecord {
	if !req.UserMessagePersisted || len(messages) == 0 {
		return messages
	}
	if persistedID := strings.TrimSpace(req.PersistedUserMessageID); persistedID != "" {
		for i := len(messages) - 1; i >= 0; i-- {
			if strings.TrimSpace(messages[i].DBMessageID) == persistedID {
				return append(messages[:i], messages[i+1:]...)
			}
		}
		return messages
	}

	targetSessionID := strings.TrimSpace(req.ThreadID)
	targetExternalID := strings.TrimSpace(req.ExternalMessageID)
	targetPlatform := strings.TrimSpace(req.CurrentChannel)
	targetSenderChannelID := strings.TrimSpace(req.SourceChannelIdentityID)
	if targetExternalID == "" {
		return messages
	}

	for i := len(messages) - 1; i >= 0; i-- {
		item := messages[i]
		if !strings.EqualFold(strings.TrimSpace(item.ModelMessage.Role), "user") {
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
		if targetSenderChannelID != "" && item.SenderChannelIdentityID != "" && item.SenderChannelIdentityID != targetSenderChannelID {
			continue
		}
		return append(messages[:i], messages[i+1:]...)
	}

	return messages
}

func estimateMessageTokens(msg ModelMessage) int {
	text := msg.TextContent()
	if len(text) == 0 {
		data, _ := json.Marshal(msg.Content)
		return contextfrag.TokensFromBytes(len(data))
	}
	return contextfrag.TokensFromBytes(len(text))
}

// totalCompactableHistoryTokens estimates the tokens held by raw history rows
// only. Active summaries are excluded: compaction can never shrink them, so
// counting them toward the compaction trigger would re-fire it on every
// request once accumulated summaries alone cross the threshold.
func totalCompactableHistoryTokens(records []historyfrag.HistoryRecord) int {
	total := 0
	for _, record := range records {
		if record.Kind == contextfrag.KindConversationSummary || record.Lifecycle == historyfrag.LifecycleActiveSummary {
			continue
		}
		total += estimateMessageTokens(record.ModelMessage)
	}
	return total
}

func estimateMessagesTokens(messages []historyfrag.HistoryRecord) int {
	total := 0
	for _, m := range messages {
		total += estimateMessageTokens(m.ModelMessage)
	}
	return total
}
