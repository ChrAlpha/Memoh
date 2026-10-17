package transcript

import (
	"context"
	"log/slog"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// Purpose names what an assembled context is for. The steps are the same for
// every purpose; the purpose only picks the source and which steps have
// anything to act on.
type Purpose int

const (
	// PurposeRun assembles the history a new run starts from.
	PurposeRun Purpose = iota
	// PurposeContinuation assembles it for a run resumed from the stored
	// transcript (decision answers, approvals).
	PurposeContinuation
	// PurposeMemoryQuery only needs the newest user messages.
	PurposeMemoryQuery
)

type Request struct {
	Purpose Purpose
	// Chat carries the retry/edit cutoff, the required row, and the persisted
	// current row. It is zero for a continuation.
	Chat     runreq.ChatRequest
	Fallback historyfrag.ScopeFallback
	// Scope is the summary scope; Scope.SessionID is the session being read.
	Scope contextfrag.Scope
	// Budget is the model's token budget. It bounds what one load may
	// materialize; zero uses the absolute cap.
	Budget int
}

type Assembled struct {
	Messages []runreq.ModelMessage
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

func ForChat(purpose Purpose, req runreq.ChatRequest, budget int) Request {
	return Request{
		Purpose:  purpose,
		Chat:     req,
		Fallback: scopeFallbackFromChatRequest(req),
		Scope:    SummaryScope(req.BotID, req.ChatID, req.ThreadID, req.ConversationType, req.ConversationName, req.ReplyTarget),
		Budget:   budget,
	}
}

// Assemble is the only way chat context is read. The source is every
// active row after the session's compaction boundary plus the live summaries;
// nothing is dropped by age or by size.
func (t *Reader) Assemble(ctx context.Context, r Request) (Assembled, error) {
	loaded, truncated, err := t.loadContextRecords(ctx, r)
	if err != nil {
		return Assembled{}, err
	}
	loaded = dropPreflightFailureTurns(loaded)
	loaded = dropEmptyHistoryFailures(loaded)
	sessionID := r.Scope.SessionID
	boundary := t.LoadArtifactBoundary(ctx, loaded, sessionID, r.Chat.HistoryCutoffBeforeMessageID)
	loaded = filterMessagesBeforeID(loaded, r.Chat.HistoryCutoffBeforeMessageID)
	loaded = dedupePersistedCurrentUserMessage(loaded, r.Chat)
	if r.Purpose == PurposeMemoryQuery {
		return Assembled{Records: loaded, Truncated: truncated}, nil
	}
	loaded, err = t.ensureRequiredHistoryMessage(ctx, loaded, r.Chat)
	if err != nil {
		return Assembled{}, err
	}
	loaded, err = t.replaceCompactedMessages(ctx, sessionID, r.Scope, loaded, boundary)
	if err != nil {
		return Assembled{}, err
	}
	loaded = projectInterruptedHistoryReasoning(loaded)
	compactable := totalCompactableHistoryTokens(loaded)
	annotated := injectWorkspaceTransitionRecords(loaded)
	messages := make([]runreq.ModelMessage, 0, len(annotated))
	for _, record := range annotated {
		messages = append(messages, record.ModelMessage)
	}
	return Assembled{
		Messages:          messages,
		Records:           annotated,
		EstimatedTokens:   estimateMessagesTokens(annotated),
		CompactableTokens: compactable,
		Truncated:         truncated,
	}, nil
}

// loadContextRecords reads the rows for one purpose. A request without a
// session has no stored history.
func (t *Reader) loadContextRecords(ctx context.Context, r Request) ([]historyfrag.HistoryRecord, bool, error) {
	sessionID := strings.TrimSpace(r.Scope.SessionID)
	if t.messages == nil || sessionID == "" {
		return nil, false, nil
	}
	var (
		msgs      []messagepkg.Message
		truncated bool
	)
	switch r.Purpose {
	case PurposeMemoryQuery:
		var err error
		msgs, err = t.messages.ListRecentUserBySession(ctx, sessionID, MemoryQueryRecentMessages)
		if err != nil {
			return nil, false, err
		}
	default:
		maxBytes := t.historyLoadMaxBytes(r.Budget)
		loaded, err := t.messages.ListActiveAfterCompactionBySessionWithinBytes(ctx, sessionID, maxBytes)
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
			t.logger.WarnContext(ctx, "context_history_load_truncated",
				slog.String("session_id", sessionID),
				slog.Int64("loaded_bytes", total),
				slog.Int64("max_bytes", maxBytes))
		}
	}
	result, err := t.recordsFromMessages(msgs, r.Fallback)
	if err != nil {
		return nil, false, err
	}
	return result, truncated, nil
}

// recordsFromMessages is where stored rows become context records. Every read
// of history goes through it, the initial load and the required-row window
// re-read alike. Rows refused at admission never reach it: each reader is a
// message-service ForModel or model-only query that leaves them out in SQL,
// before any byte budget or row limit is applied. The budget covers content
// only and metadata arrives undecoded, so each source row is released once
// converted and at most one row's metadata is decoded at a time.
func (t *Reader) recordsFromMessages(msgs []messagepkg.Message, fallback historyfrag.ScopeFallback) ([]historyfrag.HistoryRecord, error) {
	result := make([]historyfrag.HistoryRecord, 0, len(msgs))
	for i := range msgs {
		record, err := historyfrag.FromDBMessageWithLogger(t.logger, msgs[i], fallback)
		if err != nil {
			return nil, err
		}
		msgs[i] = messagepkg.Message{}
		result = append(result, record)
	}
	return result, nil
}

// historyLoadMaxBytes converts a token budget into the byte budget used for
// database-side history admission; a missing budget falls back to the
// absolute cap so no load path is ever unbounded.
func (t *Reader) historyLoadMaxBytes(contextTokenBudget int) int64 {
	if contextTokenBudget <= 0 {
		contextTokenBudget = t.AbsoluteMaxTokens()
	}
	return int64(contextTokenBudget) * contextfrag.EstimateBytesPerToken
}

const MemoryQueryRecentMessages = 4
