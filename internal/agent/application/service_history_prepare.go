package application

import (
	"context"
	"strings"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

type preparedHistoryContext struct {
	messages       []ModelMessage
	records        []historyfrag.HistoryRecord
	pressureTokens int
	// truncated reports that the load's byte budget left rows after the
	// checkpoint out, so the request must compact whatever its pressure.
	truncated bool
}

// prepareHistoryContext loads the history a request renders: the history
// under the session's checkpoint, with what it must keep raw and without
// what never replays. The checkpoint first steps over the history from
// before checkpoints that the request cannot render. An edit or retry
// renders the history as it stood at its cutoff, under the checkpoint its
// replacement keeps, which its compaction steps over from once it makes that
// checkpoint the session's.
func (s *Service) prepareHistoryContext(
	ctx context.Context,
	req ChatRequest,
	fallback historyfrag.ScopeFallback,
	contextTokenBudget int,
) (preparedHistoryContext, error) {
	cut, err := s.loadHistoryCut(ctx, req)
	if err != nil {
		return preparedHistoryContext{}, err
	}
	if cut.replacedTurnID == "" {
		if err := s.stepOverHistory(ctx, req.ThreadID, cut.before, contextTokenBudget); err != nil {
			return preparedHistoryContext{}, err
		}
	}
	scope := compactionSummaryScope(req.BotID, req.ChatID, req.ThreadID, req.ConversationType, req.ConversationName, req.ReplyTarget)
	loaded, truncated, err := s.loadHistoryRecords(ctx, fallback, scope, contextTokenBudget, cut)
	if err != nil {
		return preparedHistoryContext{}, err
	}
	loaded = pruneHistoryForGateway(loaded)
	loaded = dropPreflightFailureTurns(loaded)
	loaded = dropEmptyHistoryFailures(loaded)
	loaded = dedupePersistedCurrentUserMessage(loaded, req)
	loaded, err = s.ensureRequiredHistoryMessage(ctx, loaded, req)
	if err != nil {
		return preparedHistoryContext{}, err
	}
	loaded = projectInterruptedHistoryReasoning(loaded)
	messages, records := historyMessagesFromRecords(loaded)
	return preparedHistoryContext{
		messages:       messages,
		records:        records,
		pressureTokens: estimateMessagesTokens(loaded),
		truncated:      truncated,
	}, nil
}

func historyErrorCode(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	code, _ := meta[messagepkg.HistoryErrorCodeMetadataKey].(string)
	return strings.TrimSpace(code)
}

// dropPreflightFailureTurns removes inputs rejected before the agent accepted
// them. They stay in the UI, but a later ordinary message must not smuggle a
// Hook-rejected input into the model context.
func dropPreflightFailureTurns(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(records) == 0 {
		return records
	}
	out := make([]historyfrag.HistoryRecord, 0, len(records))
	for _, rec := range records {
		if rec.Metadata != nil && rec.Metadata[messagepkg.HistoryFailureOriginMetadataKey] == messagepkg.HistoryFailureOriginUserMessageHook {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// dropEmptyHistoryFailures removes empty assistant rows that exist only to
// mark a timeout/interrupt. They stay in the UI, but replaying them as
// empty assistant turns is what made the next provider call 400.
func dropEmptyHistoryFailures(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	if len(records) == 0 {
		return records
	}
	out := make([]historyfrag.HistoryRecord, 0, len(records))
	for _, rec := range records {
		if strings.EqualFold(strings.TrimSpace(rec.ModelMessage.Role), "assistant") &&
			historyErrorCode(rec.Metadata) != "" &&
			isEmptyAssistantMessage(rec.ModelMessage) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// projectInterruptedHistoryReasoning turns interrupted reasoning into assistant
// text. Durable history keeps the typed reasoning part; every checkpoint is
// projected the same way on every turn, so a superseded one neither rewrites
// the replayed prefix nor reaches the provider as unsigned reasoning.
func projectInterruptedHistoryReasoning(records []historyfrag.HistoryRecord) []historyfrag.HistoryRecord {
	var projected []historyfrag.HistoryRecord
	for i, record := range records {
		if !strings.EqualFold(strings.TrimSpace(record.ModelMessage.Role), "assistant") ||
			record.Metadata[messagepkg.AgentStepInterruptedMetadataKey] != true {
			continue
		}
		if projected == nil {
			projected = append([]historyfrag.HistoryRecord(nil), records...)
		}
		projected[i].ModelMessage = timeline.ProjectInterruptedReasoning(record.ModelMessage)
	}
	if projected == nil {
		return records
	}
	return projected
}
