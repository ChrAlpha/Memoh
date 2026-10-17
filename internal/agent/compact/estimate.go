package compact

import (
	"context"
	"strings"

	"github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/db"
)

// compactableTokens prices the raw history rows a compaction could
// claim, in the units of the request pressure they are judged against.
// Active summaries are not compactable and are left out.
func compactableTokens(records []historyfrag.HistoryRecord) int {
	rows := make([]historyfrag.HistoryRecord, 0, len(records))
	for _, record := range records {
		if record.Kind == contextfrag.KindConversationSummary || record.Lifecycle == historyfrag.LifecycleActiveSummary {
			continue
		}
		rows = append(rows, record)
	}
	return runreq.RequestInputTokens(0, stepMessages(rows))
}

// fixedRequestTokens is the part of a judged request that no compaction can
// shrink: the pressure less the rows it was measured from. It holds the system
// prompt and tool definitions, this request's own messages, and the summaries.
func fixedRequestTokens(pressure int, records []historyfrag.HistoryRecord) int {
	return max(0, pressure-compactableTokens(records))
}

// lastRunFixed is what the session's latest run spent on its system
// prompt and tool definitions, read from its context lifecycle. A turn judges
// its pressure before either is built, so the previous run's measurement
// stands in for them; a session that has none has no history to compact
// either.
func (c *Compactor) lastRunFixed(ctx context.Context, sessionID string) int {
	parsed, err := db.ParseUUID(sessionID)
	if err != nil {
		return 0
	}
	raw, err := c.queries.GetLatestContextLifecycleBySession(ctx, parsed)
	if err != nil {
		return 0
	}
	snapshot, err := contextfrag.DecodeLifecycleSnapshot(raw)
	if err != nil {
		return 0
	}
	return runreq.LifecycleFixedTokens(snapshot)
}

// Estimate prices the request a turn is about to make with
// whatever history it ends up assembled from. Everything but the history is
// fixed: the system prompt and tools, and this turn's own messages (fork
// context, notices, recalled memory, requested skills, the user's message).
type Estimate struct {
	compactor *Compactor
	ctx       context.Context
	sessionID string
	// own is this turn's messages that no history row holds.
	own []sdk.Message
	// systemAndTools is read on first use: a turn with nothing to compact
	// never needs it.
	systemAndTools      int
	systemAndToolsKnown bool
}

// NewEstimate starts the estimate of the request a turn on the session is about
// to make. own is the turn's messages that no history row holds.
func (c *Compactor) NewEstimate(ctx context.Context, sessionID string, own []sdk.Message) *Estimate {
	return &Estimate{compactor: c, ctx: ctx, sessionID: sessionID, own: own}
}

// Tokens is the request's size with history as its conversation.
func (e *Estimate) Tokens(history []runreq.ModelMessage) int {
	if !e.systemAndToolsKnown {
		e.systemAndTools = e.compactor.lastRunFixed(e.ctx, e.sessionID)
		e.systemAndToolsKnown = true
	}
	return runreq.RequestInputTokens(e.systemAndTools, e.Conversation(history))
}

// Conversation is the turn's messages with history.
func (e *Estimate) Conversation(history []runreq.ModelMessage) []sdk.Message {
	return append(runreq.ModelMessagesToSDKMessages(history), e.own...)
}

// UserMessageEstimate stands in for the user's message when it is not
// a stored row yet: its text and one image part per image attachment, which is
// how the estimator prices them.
func UserMessageEstimate(req runreq.ChatRequest, modelQuery string) []sdk.Message {
	if req.ReusePersistedUserMessage {
		return nil
	}
	parts := []sdk.MessagePart{sdk.TextPart{Text: modelQuery}}
	for _, attachment := range req.Attachments {
		if strings.EqualFold(strings.TrimSpace(attachment.Type), "image") {
			parts = append(parts, sdk.ImagePart{})
		}
	}
	return []sdk.Message{{Role: sdk.MessageRoleUser, Content: parts}}
}
