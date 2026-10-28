package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/agentapi"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func injectWorkspaceTransitionRecords(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(records) == 0 {
		return records
	}
	result := make([]historyfrag.HistoryRecord, 0, len(records)+2)
	var previous *agentapi.WorkspaceTarget
	for _, record := range records {
		if current := workspaceTargetFromMetadata(record.Metadata); current != nil && !sameWorkspaceTarget(previous, current) {
			text := renderWorkspaceTransition(previous, current)
			result = append(result, historyfrag.HistoryRecord{
				ModelMessage: runreq.ModelMessage{Role: "system", Content: runreq.NewTextContent(text)},
				Synthetic:    true,
			})
			previous = current
		}
		result = append(result, record)
	}
	return result
}

// injectRuntimeContextRecords replays, right after each request row, the
// runtime context its request carried (the memory its turn recalled, the
// skills its user requested), as the request sent it. The run's own request,
// when the history holds it, stays bare: the run supplies its context anew.
func injectRuntimeContextRecords(records []historyfrag.HistoryRecord, currentRequestID string) []historyfrag.HistoryRecord {
	var out []historyfrag.HistoryRecord
	for i, record := range records {
		items := historyfrag.RuntimeContextFromMetadata(record.DBMessageID, record.Metadata)
		if len(items) == 0 || (currentRequestID != "" && record.DBMessageID == currentRequestID) {
			if out != nil {
				out = append(out, record)
			}
			continue
		}
		if out == nil {
			out = append(make([]historyfrag.HistoryRecord, 0, len(records)+len(items)), records[:i]...)
		}
		out = append(out, record)
		for _, item := range items {
			out = append(out, historyfrag.HistoryRecord{
				ModelMessage: item.Message,
				Synthetic:    true,
				Metadata:     map[string]any{messagepkg.RuntimeContextMetadataKey: item.Kind},
				Scope:        record.Scope,
			})
		}
	}
	if out == nil {
		return records
	}
	return out
}

func renderWorkspaceTransition(previous, current *agentapi.WorkspaceTarget) string {
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

func sameWorkspaceTarget(left, right *agentapi.WorkspaceTarget) bool {
	if left == nil || right == nil {
		return left == right
	}
	return strings.TrimSpace(left.TargetID) == strings.TrimSpace(right.TargetID) &&
		strings.TrimSpace(left.Kind) == strings.TrimSpace(right.Kind)
}

func workspaceTargetFromMetadata(metadata map[string]any) *agentapi.WorkspaceTarget {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["execution_location"].(map[string]any)
	if !ok {
		return nil
	}
	target := &agentapi.WorkspaceTarget{
		TargetID: strings.TrimSpace(runreq.ReadAnyString(raw["target_id"])),
		Kind:     strings.TrimSpace(runreq.ReadAnyString(raw["kind"])),
		Name:     strings.TrimSpace(runreq.ReadAnyString(raw["name"])),
	}
	if target.TargetID == "" {
		return nil
	}
	if target.Name == "" {
		target.Name = target.TargetID
	}
	return target
}

func (t *Reader) CurrentWorkspaceMessage(ctx context.Context, req runreq.ChatRequest) *runreq.ModelMessage {
	current := req.WorkspaceTarget
	if current == nil || strings.TrimSpace(current.TargetID) == "" {
		return nil
	}
	var previous *agentapi.WorkspaceTarget
	if strings.TrimSpace(req.ThreadID) != "" {
		if sess, err := t.sessions.Get(ctx, req.ThreadID); err == nil {
			if raw, ok := sess.Metadata["workspace_target"].(map[string]any); ok {
				previous = workspaceTargetFromMetadata(map[string]any{"execution_location": raw})
			}
		}
	}
	text := renderWorkspaceTransition(previous, current)
	if text == "" {
		return nil
	}
	message := runreq.ModelMessage{Role: "system", Content: runreq.NewTextContent(text)}
	return &message
}

// ensureRequiredHistoryMessage keeps the request's own row in the context when
// the load left it out (compacted away or past the byte budget) by re-reading
// the window from that row. A required row refused at admission is not in the
// context by the same rule as any other row, so the run fails as for a row that
// is not visible.
func (t *Reader) ensureRequiredHistoryMessage(ctx context.Context, messages []historyfrag.HistoryRecord, req runreq.ChatRequest) ([]historyfrag.HistoryRecord, error) {
	messageID := strings.TrimSpace(req.RequiredHistoryMessageID)
	if messageID == "" || strings.TrimSpace(req.ThreadID) == "" {
		return messages, nil
	}
	for i, item := range messages {
		if strings.TrimSpace(item.DBMessageID) == messageID {
			messages[i].Required = true
			return messages, nil
		}
	}
	window, err := t.messages.ListVisibleFromForModelBySession(ctx, req.ThreadID, messageID)
	if err != nil {
		return nil, err
	}
	required, err := t.recordsFromMessages(window)
	if err != nil {
		return nil, err
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

func SummaryScope(botID, chatID, sessionID, conversationType, conversationName, replyTarget string) contextfrag.Scope {
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
func dedupePersistedCurrentUserMessage(messages []historyfrag.HistoryRecord, req runreq.ChatRequest) []historyfrag.HistoryRecord {
	if !req.UserMessagePersisted || req.RequestStaysInContext || len(messages) == 0 {
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

func EstimateMessageTokens(msg runreq.ModelMessage) int {
	text := msg.TextContent()
	if len(text) == 0 {
		data, _ := json.Marshal(msg.Content)
		return contextfrag.TokensFromBytes(len(data))
	}
	return contextfrag.TokensFromBytes(len(text) + replayedPartBytes(msg.Content))
}

// replayedPartBytes measures the reasoning and tool exchange parts that ride
// along with assistant text; TextContent alone would leave them uncounted.
// Inline images stay out so the selector's fixed image pricing still applies.
func replayedPartBytes(content json.RawMessage) int {
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return 0
	}
	total := 0
	for _, raw := range parts {
		var part struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &part) != nil {
			continue
		}
		switch part.Type {
		case "reasoning", "tool-call", "tool-result":
			total += len(raw)
		}
	}
	return total
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
		total += EstimateMessageTokens(record.ModelMessage)
	}
	return total
}

func estimateMessagesTokens(messages []historyfrag.HistoryRecord) int {
	total := 0
	for _, m := range messages {
		total += EstimateMessageTokens(m.ModelMessage)
	}
	return total
}
