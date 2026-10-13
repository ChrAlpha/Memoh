package application

import (
	"context"
	"log/slog"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// contextIncludesUnadmittedPassive decides whether rows refused at admission
// (sender not allowed by ACL, or refused by the external runtime driver gate)
// are shown to the model. Default false, so chat and discuss agree. Changing it
// is a product decision, not a per-call switch.
const contextIncludesUnadmittedPassive = false

// contextPurpose names what an assembled context is for. The steps are the
// same for every purpose; the purpose only picks the source and which steps
// have anything to act on.
type contextPurpose int

const (
	// contextPurposeRun assembles the history a new run starts from.
	contextPurposeRun contextPurpose = iota
	// contextPurposeContinuation assembles it for a run resumed from the
	// stored transcript (decision answers, approvals).
	contextPurposeContinuation
	// contextPurposeMemoryQuery only needs the newest user messages.
	contextPurposeMemoryQuery
)

type contextRequest struct {
	Purpose contextPurpose
	// Chat carries the retry/edit cutoff, the required row, and the persisted
	// current row. It is zero for a continuation.
	Chat     ChatRequest
	Fallback historyfrag.ScopeFallback
	// Scope is the summary scope; Scope.SessionID is the session being read.
	Scope contextfrag.Scope
	// Budget is the model's token budget. It bounds what one load may
	// materialize; zero uses the absolute cap.
	Budget int
}

type assembledContext struct {
	Messages []ModelMessage
	Records  []historyfrag.HistoryRecord
	// EstimatedTokens is the estimate for Messages, markers included.
	EstimatedTokens int
	// CompactableTokens counts raw rows only. Active summaries can never be
	// compacted away, so counting them would re-fire the trigger on every
	// request once accumulated summaries alone cross the threshold.
	CompactableTokens int
	// Truncated reports that the byte-budgeted load may have stopped before
	// the oldest row, so the raw history is larger than what was loaded.
	Truncated bool
}

func chatContextRequest(purpose contextPurpose, req ChatRequest, budget int) contextRequest {
	return contextRequest{
		Purpose:  purpose,
		Chat:     req,
		Fallback: historyScopeFallbackFromChatRequest(req),
		Scope:    compactionSummaryScope(req.BotID, req.ChatID, req.ThreadID, req.ConversationType, req.ConversationName, req.ReplyTarget),
		Budget:   budget,
	}
}

// assembleContext is the only way chat context is read. The source is every
// active row after the session's compaction boundary plus the live summaries;
// nothing is dropped by age or by size.
func (s *Service) assembleContext(ctx context.Context, r contextRequest) (assembledContext, error) {
	loaded, truncated, err := s.loadContextRecords(ctx, r)
	if err != nil {
		return assembledContext{}, err
	}
	loaded = dropPreflightFailureTurns(loaded)
	loaded = dropEmptyHistoryFailures(loaded)
	sessionID := r.Scope.SessionID
	boundary := s.loadCompactionArtifactBoundary(ctx, loaded, sessionID, r.Chat.HistoryCutoffBeforeMessageID)
	loaded = filterMessagesBeforeID(loaded, r.Chat.HistoryCutoffBeforeMessageID)
	loaded = dedupePersistedCurrentUserMessage(loaded, r.Chat)
	if r.Purpose == contextPurposeMemoryQuery {
		return assembledContext{Records: loaded, Truncated: truncated}, nil
	}
	loaded, err = s.ensureRequiredHistoryMessage(ctx, loaded, r.Chat)
	if err != nil {
		return assembledContext{}, err
	}
	loaded, err = s.replaceCompactedMessages(ctx, sessionID, r.Scope, loaded, boundary)
	if err != nil {
		return assembledContext{}, err
	}
	loaded = projectInterruptedHistoryReasoning(loaded)
	compactable := totalCompactableHistoryTokens(loaded)
	annotated := injectWorkspaceTransitionRecords(loaded)
	messages := make([]ModelMessage, 0, len(annotated))
	for _, record := range annotated {
		messages = append(messages, record.ModelMessage)
	}
	return assembledContext{
		Messages:          messages,
		Records:           annotated,
		EstimatedTokens:   estimateMessagesTokens(annotated),
		CompactableTokens: compactable,
		Truncated:         truncated,
	}, nil
}

// loadContextRecords reads the rows for one purpose. A request without a
// session has no stored history.
func (s *Service) loadContextRecords(ctx context.Context, r contextRequest) ([]historyfrag.HistoryRecord, bool, error) {
	sessionID := strings.TrimSpace(r.Scope.SessionID)
	if s.messageService == nil || sessionID == "" {
		return nil, false, nil
	}
	var (
		msgs      []messagepkg.Message
		truncated bool
	)
	switch r.Purpose {
	case contextPurposeMemoryQuery:
		var err error
		msgs, err = s.messageService.ListRecentUserBySession(ctx, sessionID, defaultMemoryQueryRecentMessages)
		if err != nil {
			return nil, false, err
		}
	default:
		maxBytes := s.historyLoadMaxBytes(r.Budget)
		loaded, err := s.messageService.ListActiveAfterCompactionBySessionWithinBytes(ctx, sessionID, maxBytes)
		if err != nil {
			return nil, false, err
		}
		msgs = loaded.Messages
		var total int64
		for i := range msgs {
			total += int64(len(msgs[i].Content))
		}
		// The load keeps the row that crosses the budget, so a total at or
		// over it means older rows may have been left behind.
		truncated = total >= maxBytes
		if truncated {
			s.logger.WarnContext(ctx, "context_history_load_truncated",
				slog.String("session_id", sessionID),
				slog.Int64("loaded_bytes", total),
				slog.Int64("max_bytes", maxBytes))
		}
	}
	// The budget covers content only; metadata arrives undecoded. Release each
	// row once converted so at most one row's metadata is decoded at a time.
	result := make([]historyfrag.HistoryRecord, 0, len(msgs))
	for i := range msgs {
		if !contextIncludesUnadmittedPassive && msgs[i].Admission() != "" {
			msgs[i] = messagepkg.Message{}
			continue
		}
		record, err := historyfrag.FromDBMessageWithLogger(s.logger, msgs[i], r.Fallback)
		if err != nil {
			return nil, false, err
		}
		msgs[i] = messagepkg.Message{}
		result = append(result, record)
	}
	return result, truncated, nil
}

// historyLoadMaxBytes converts a token budget into the byte budget used for
// database-side history admission; a missing budget falls back to the
// absolute cap so no load path is ever unbounded.
func (s *Service) historyLoadMaxBytes(contextTokenBudget int) int64 {
	if contextTokenBudget <= 0 {
		contextTokenBudget = s.contextAbsoluteMaxTokens()
	}
	return int64(contextTokenBudget) * contextfrag.EstimateBytesPerToken
}
