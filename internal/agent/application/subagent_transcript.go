package application

import (
	"context"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	tools "github.com/felinics/memoh/internal/agent/tool"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// LoadSubagentTranscript reads the transcript a parent-driven subagent task
// continues through the window a direct turn on the same thread reads, so a
// long-lived agent replays its newest history inside the model's budget and
// its compaction summaries in place of the rows they replaced.
func (s *Service) LoadSubagentTranscript(ctx context.Context, req tools.SubagentTranscriptRequest) ([]sdk.Message, error) {
	budget := s.contextAbsoluteMaxTokens()
	if req.ContextTokenBudget > 0 {
		budget = min(req.ContextTokenBudget, budget)
	}
	chatReq := ChatRequest{
		BotID:            req.BotID,
		ChatID:           req.ChatID,
		ThreadID:         req.SessionID,
		SessionType:      sessionpkg.TypeSubagent,
		ConversationType: req.ConversationType,
		ReplyTarget:      req.ReplyTarget,
	}
	prepared, err := s.prepareHistoryContext(ctx, chatReq, historyScopeFallbackFromChatRequest(chatReq), budget)
	if err != nil {
		return nil, err
	}
	messages := withoutHistoryMessage(prepared.messages, prepared.records, req.RequestMessageID)
	messages, _, _ = normalizeContextMessages(messages, nil, nil)
	return modelMessagesToSDKMessages(messages), nil
}

// withoutHistoryMessage drops the row persisted as messageID from a history
// window whose records run parallel to its messages.
func withoutHistoryMessage(messages []ModelMessage, records []historyfrag.HistoryRecord, messageID string) []ModelMessage {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return messages
	}
	for i := len(records) - 1; i >= 0; i-- {
		if strings.TrimSpace(records[i].DBMessageID) == messageID {
			return append(messages[:i:i], messages[i+1:]...)
		}
	}
	return messages
}
