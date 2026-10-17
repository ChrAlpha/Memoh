package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

type switchQueries struct {
	*controllerQueries
	left      int64
	measured  sqlc.MeasureUncompactedMessagesBySessionParams
	summaries []sqlc.BotHistoryMessageCompact
}

func (q *switchQueries) MeasureUncompactedMessagesBySession(_ context.Context, arg sqlc.MeasureUncompactedMessagesBySessionParams) (sqlc.MeasureUncompactedMessagesBySessionRow, error) {
	q.measured = arg
	return sqlc.MeasureUncompactedMessagesBySessionRow{CandidateCount: q.left}, nil
}

func (q *switchQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return q.summaries, nil
}

type failingCompactionRunner struct{ err error }

func (r failingCompactionRunner) RunCompactionSync(context.Context, compaction.TriggerConfig) (compaction.Result, error) {
	return compaction.Result{}, r.err
}

func switchFixture(t *testing.T) (*agentStepCommitter, *recordingCompactionRunner, *switchQueries, time.Time) {
	t.Helper()
	service, runner := newControllerPolicyService(t)
	q := &switchQueries{controllerQueries: service.queries.(*controllerQueries), left: 1}
	service.queries = q
	start := time.UnixMilli(1_700_000_000_000).UTC()
	q.summaries = []sqlc.BotHistoryMessageCompact{{
		ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
		BotID:     compactionConfigUUID(t, syncCompactBotID),
		SessionID: compactionConfigUUID(t, syncCompactThreadID),
		Status:    "ok", Summary: "steps so far", Coverage: []byte("[]"),
		AnchorStartMs: start.UnixMilli(), AnchorEndMs: start.Add(time.Second).UnixMilli(),
	}}
	committer := &agentStepCommitter{
		service: service,
		req:     ChatRequest{BotID: syncCompactBotID, ChatID: syncCompactBotID, ThreadID: syncCompactThreadID, RunID: uuid.NewString(), Query: "build the report"},
		stepRowsAt: map[int]time.Time{
			0: start, 1: start.Add(time.Second), 2: start.Add(2 * time.Second),
		},
		inputID: "input-row",
	}
	return committer, runner, q, start
}

func TestContextSwitchFoldsTheRunsStepsBeforeTheLatest(t *testing.T) {
	committer, runner, q, start := switchFixture(t)
	summary, err := committer.switchContext(t.Context(), native.ContextSwitchInput{LatestStep: 2, KeptTokens: 3000, AllowanceTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if text := sdkMessagesToModelMessages([]sdk.Message{summary})[0].TextContent(); !strings.Contains(text, "steps so far") {
		t.Fatalf("summary message = %q", text)
	}
	if len(runner.configs) != 1 {
		t.Fatalf("compactions=%d", len(runner.configs))
	}
	cfg := runner.configs[0]
	want := turn.ContextMessageSource{Kind: "history", ID: "input-row", Current: true}
	if cfg.RunID != committer.req.RunID || !cfg.CandidatesFrom.Equal(start) || !cfg.CandidatesBefore.Equal(start.Add(2*time.Second)) ||
		cfg.HistoryBudgetTokens != 1000 || cfg.CurrentRequest != "build the report" || len(cfg.ProtectedSources) != 1 || cfg.ProtectedSources[0] != want {
		t.Fatalf("compaction config = %+v", cfg)
	}
	if q.measured.RunID != compactionConfigUUID(t, committer.req.RunID) || !q.measured.CreatedBefore.Time.Equal(start.Add(2*time.Second)) {
		t.Fatalf("coverage check = %+v, want the folded window of this run", q.measured)
	}
}

func TestContextSwitchFailsWhenItCannotSummarizeEveryFoldedStep(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input native.ContextSwitchInput
		setup func(*agentStepCommitter, *switchQueries)
	}{
		{name: "summarizer failed", setup: func(c *agentStepCommitter, _ *switchQueries) {
			c.service.compactionService = failingCompactionRunner{err: errors.New("summarizer down")}
		}},
		{name: "rows left raw", setup: func(_ *agentStepCommitter, q *switchQueries) { q.left = 2 }},
		{name: "no summary in the window", setup: func(_ *agentStepCommitter, q *switchQueries) { q.summaries = nil }},
		{name: "latest step stored no rows", input: native.ContextSwitchInput{LatestStep: 3, KeptTokens: 3000, AllowanceTokens: 4000}},
		{name: "no room for a summary", input: native.ContextSwitchInput{LatestStep: 2, KeptTokens: 4000, AllowanceTokens: 4000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committer, _, q, _ := switchFixture(t)
			if tc.setup != nil {
				tc.setup(committer, q)
			}
			input := tc.input
			if input.AllowanceTokens == 0 {
				input = native.ContextSwitchInput{LatestStep: 2, KeptTokens: 3000, AllowanceTokens: 4000}
			}
			if _, err := committer.switchContext(t.Context(), input); !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
				t.Fatalf("err=%v, want budget_unsatisfied", err)
			}
		})
	}
}
