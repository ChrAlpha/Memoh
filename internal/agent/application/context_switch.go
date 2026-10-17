package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// switchContext folds the rows this run stored for its steps before
// input.LatestStep into one summary and returns it as the run will read it
// back next turn. Any failure ends the run with budget_unsatisfied.
func (c *agentStepCommitter) switchContext(ctx context.Context, input native.ContextSwitchInput) (sdk.Message, error) {
	summary, err := c.foldSteps(ctx, input)
	if err != nil {
		return sdk.Message{}, fmt.Errorf("%w: in-run compaction: %w", contextfrag.ErrBudgetUnsatisfied, err)
	}
	return summary, nil
}

func (c *agentStepCommitter) foldSteps(ctx context.Context, input native.ContextSwitchInput) (sdk.Message, error) {
	c.mu.Lock()
	before, ok := c.stepRowsAt[input.LatestStep]
	var from time.Time
	for _, at := range c.stepRowsAt {
		if from.IsZero() || at.Before(from) {
			from = at
		}
	}
	inputID := c.inputID
	c.mu.Unlock()
	if !ok || !from.Before(before) {
		return sdk.Message{}, errors.New("the steps to fold have no stored rows")
	}
	room := input.AllowanceTokens - input.KeptTokens
	if room <= 0 {
		return sdk.Message{}, fmt.Errorf("no room for a summary: kept=%d allowance=%d", input.KeptTokens, input.AllowanceTokens)
	}
	s := c.service
	cfg, err := s.compactionConfig(ctx, c.req, input.KeptTokens, c.rc.model.ID)
	if err != nil {
		return sdk.Message{}, err
	}
	cfg.RunID = c.req.RunID
	cfg.CandidatesFrom, cfg.CandidatesBefore = from, before
	cfg.HistoryBudgetTokens = room
	left := int64(0)
	if inputID != "" {
		cfg.ProtectedSources = []turn.ContextMessageSource{{Kind: "history", ID: inputID, Current: true}}
		left = 1
	}
	if err := s.runCompaction(ctx, cfg); err != nil {
		return sdk.Message{}, err
	}

	runID, err := db.ParseUUID(c.req.RunID)
	if err != nil {
		return sdk.Message{}, err
	}
	sessionID, err := db.ParseUUID(c.req.ThreadID)
	if err != nil {
		return sdk.Message{}, err
	}
	measure, err := s.queries.MeasureUncompactedMessagesBySession(ctx, sqlc.MeasureUncompactedMessagesBySessionParams{
		SessionID:     sessionID,
		CreatedFrom:   pgtype.Timestamptz{Time: from, Valid: true},
		CreatedBefore: pgtype.Timestamptz{Time: before, Valid: true},
		RunID:         runID,
	})
	if err != nil {
		return sdk.Message{}, err
	}
	if measure.CandidateCount > left {
		return sdk.Message{}, fmt.Errorf("%d stored rows of the folded steps were left out of the summary", measure.CandidateCount-left)
	}
	frontier, err := s.loadActiveCompactionFrontier(ctx, c.req.BotID, c.req.ThreadID)
	if err != nil {
		return sdk.Message{}, err
	}
	var folded []compaction.Artifact
	for _, artifact := range frontier.Artifacts {
		if artifact.AnchorStartMs >= from.UnixMilli() && artifact.AnchorEndMs < before.UnixMilli() {
			folded = append(folded, artifact)
		}
	}
	if len(folded) != 1 {
		return sdk.Message{}, fmt.Errorf("want one summary of the folded steps, found %d", len(folded))
	}
	record := folded[0].HistoryRecord(contextfrag.Scope{BotID: c.req.BotID, SessionID: c.req.ThreadID})
	return modelMessagesToSDKMessages([]ModelMessage{record.ModelMessage})[0], nil
}
