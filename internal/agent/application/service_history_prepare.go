package application

import (
	"context"

	historyfrag "github.com/memohai/memoh/internal/agent/context/history"
)

type preparedHistoryContext struct {
	messages          []ModelMessage
	records           []historyfrag.HistoryRecord
	estimatedTokens   int
	compactableTokens int
}

func (s *Service) prepareHistoryContext(
	ctx context.Context,
	req ChatRequest,
	fallback historyfrag.ScopeFallback,
	contextTokenBudget int,
) (preparedHistoryContext, error) {
	loaded, err := s.loadHistoryRecords(ctx, fallback, req.ThreadID, defaultMaxContextMinutes)
	if err != nil {
		return preparedHistoryContext{}, err
	}
	loaded = pruneHistoryForGateway(loaded)
	boundary := s.loadCompactionArtifactBoundary(ctx, loaded, req.ThreadID, req.HistoryCutoffBeforeMessageID)
	loaded = filterMessagesBeforeID(loaded, req.HistoryCutoffBeforeMessageID)
	loaded = dedupePersistedCurrentUserMessage(loaded, req)
	loaded, err = s.ensureRequiredHistoryMessage(ctx, loaded, req)
	if err != nil {
		return preparedHistoryContext{}, err
	}
	loaded, err = s.replaceCompactedMessages(
		ctx,
		req.ThreadID,
		compactionSummaryScope(req.BotID, req.ChatID, req.ThreadID, req.ConversationType, req.ConversationName, req.ReplyTarget),
		loaded,
		boundary,
	)
	if err != nil {
		return preparedHistoryContext{}, err
	}
	compactableTokens := totalCompactableHistoryTokens(loaded)
	_, records, _ := trimMessagesAndRecordsByTokens(s.logger, loaded, contextTokenBudget)
	messages, records, estimatedTokens := finishPreparedHistory(records)
	return preparedHistoryContext{
		messages:          messages,
		records:           records,
		estimatedTokens:   estimatedTokens,
		compactableTokens: compactableTokens,
	}, nil
}

// finishPreparedHistory derives workspace-transition markers from the records
// that actually survived trimming, so a marker can never outlive or orphan
// the message it annotates: trimming first, deriving second keeps the pair
// atomic by construction.
func finishPreparedHistory(records []historyfrag.HistoryRecord) ([]ModelMessage, []historyfrag.HistoryRecord, int) {
	records = injectWorkspaceTransitionRecords(records)
	estimated := 0
	for _, record := range records {
		estimated += estimateMessageTokens(record.ModelMessage)
	}
	return historyfrag.ToModelMessages(records), records, estimated
}
