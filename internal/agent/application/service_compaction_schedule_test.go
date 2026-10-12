package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

const (
	scheduleBotID     = "00000000-0000-0000-0000-000000000453"
	scheduleSessionID = "00000000-0000-0000-0000-000000000454"
	scheduleModelID   = "00000000-0000-0000-0000-000000000451"
)

// gatedCompactionRunner stands in for the summarizer: each pass reports its
// config and then blocks until the test releases it.
type gatedCompactionRunner struct {
	started chan compaction.TriggerConfig
	release chan struct{}
}

func newGatedCompactionRunner() *gatedCompactionRunner {
	return &gatedCompactionRunner{
		started: make(chan compaction.TriggerConfig, 8),
		release: make(chan struct{}),
	}
}

func (r *gatedCompactionRunner) RunCompactionSync(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	r.started <- cfg
	select {
	case <-r.release:
		return compaction.Result{Status: compaction.StatusNoop}, nil
	case <-ctx.Done():
		return compaction.Result{}, ctx.Err()
	}
}

func (r *gatedCompactionRunner) awaitPass(t *testing.T) compaction.TriggerConfig {
	t.Helper()
	select {
	case cfg := <-r.started:
		return cfg
	case <-time.After(2 * time.Second):
		t.Fatal("no background compaction pass started")
		return compaction.TriggerConfig{}
	}
}

func (r *gatedCompactionRunner) assertNoPass(t *testing.T) {
	t.Helper()
	r.assertNoPassWithin(t, 50*time.Millisecond)
}

func (r *gatedCompactionRunner) assertNoPassWithin(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case cfg := <-r.started:
		t.Fatalf("unexpected compaction pass: %+v", cfg)
	case <-time.After(window):
	}
}

type scheduleQueries struct {
	*controllerQueries
}

func (*scheduleQueries) GetSessionByID(context.Context, pgtype.UUID) (sqlc.BotSession, error) {
	return sqlc.BotSession{}, pgx.ErrNoRows
}

func (*scheduleQueries) GetBotByID(context.Context, pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return sqlc.GetBotByIDRow{}, pgx.ErrNoRows
}

// newScheduleService wires a turn resolve against a 4000-token chat model,
// which also serves as its own summarizer.
func newScheduleService(t *testing.T, history []messagepkg.Message) (*Service, *gatedCompactionRunner) {
	t.Helper()
	policy, _ := newControllerPolicyService(t, nil)
	queries := policy.queries.(*controllerQueries)
	queries.model.Config = []byte(`{"context_window":4000}`)
	queries.settings.ChatModelID = compactionConfigUUID(t, scheduleModelID)
	runner := newGatedCompactionRunner()
	service := &Service{
		logger:            slog.New(slog.DiscardHandler),
		modelsService:     models.NewService(slog.New(slog.DiscardHandler), &scheduleQueries{queries}),
		queries:           &scheduleQueries{queries},
		settingsService:   settings.NewService(slog.New(slog.DiscardHandler), queries, nil, nil),
		messageService:    &memoryQueryMessageService{messages: history},
		compactionService: runner,
	}
	t.Cleanup(func() { close(runner.release) })
	return service, runner
}

func historyAtTokens(t *testing.T, tokens int) []messagepkg.Message {
	t.Helper()
	half := strings.Repeat("x", tokens*2)
	return []messagepkg.Message{
		persistedHistoryMessage(t, "user-1", "user", half),
		persistedHistoryMessage(t, "assistant-1", "assistant", half),
	}
}

func resolveWithin(t *testing.T, service *Service, req ChatRequest) {
	t.Helper()
	resolveWithinContext(t, context.Background(), service, req)
}

func resolveWithinContext(t *testing.T, ctx context.Context, service *Service, req ChatRequest) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := service.resolve(ctx, req)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolve waited for the summarizer")
	}
}

// A turn whose history already sits past the hard share of the window reaches
// the model at once; the compaction it needs runs behind it.
func TestResolveAtHardPressureDoesNotWaitForCompaction(t *testing.T) {
	t.Parallel()

	service, runner := newScheduleService(t, historyAtTokens(t, 3200))
	resolveWithin(t, service, ChatRequest{BotID: scheduleBotID, ChatID: scheduleBotID, ThreadID: scheduleSessionID, Query: "next"})

	cfg := runner.awaitPass(t)
	if cfg.SessionID != scheduleSessionID || !cfg.HardPressure || !cfg.AllowFrontierFusion || cfg.TotalInputTokens < 3000 {
		t.Fatalf("background pass = %+v, want a hard-pressure pass for the session", cfg)
	}
	if cfg.TargetTokens != 1600 || cfg.ContextWindowTokens != 4000 {
		t.Fatalf("background pass target=%d window=%d, want 1600/4000", cfg.TargetTokens, cfg.ContextWindowTokens)
	}
}

// The soft share schedules the same background pass without the hard-pressure
// retry schedule; below it nothing runs.
func TestResolveSchedulesCompactionFromSoftThreshold(t *testing.T) {
	t.Parallel()

	service, runner := newScheduleService(t, historyAtTokens(t, 2200))
	resolveWithin(t, service, ChatRequest{BotID: scheduleBotID, ChatID: scheduleBotID, ThreadID: scheduleSessionID, Query: "next"})
	if cfg := runner.awaitPass(t); cfg.HardPressure {
		t.Fatalf("soft-share pass = %+v, want no hard-pressure schedule", cfg)
	}

	quiet, quietRunner := newScheduleService(t, historyAtTokens(t, 1000))
	resolveWithin(t, quiet, ChatRequest{BotID: scheduleBotID, ChatID: scheduleBotID, ThreadID: scheduleSessionID, Query: "next"})
	quietRunner.assertNoPass(t)
}

func (*scheduleQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

// gatedAgentStreamer holds a discuss turn inside its model call until released.
type gatedAgentStreamer struct {
	started chan struct{}
	release chan struct{}
}

func (g *gatedAgentStreamer) Stream(ctx context.Context, _ native.RunConfig) <-chan native.StreamEvent {
	ch := make(chan native.StreamEvent, 1)
	close(g.started)
	go func() {
		defer close(ch)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
		ch <- native.StreamEvent{Type: native.EventAgentEnd, Messages: json.RawMessage(`[{"role":"assistant","content":"done"}]`)}
	}()
	return ch
}

// A discuss turn under hard pressure still runs the model; the compaction it
// needs starts once the run ends instead of ending the run to recompose.
func TestDiscussAtHardPressureRunsTheModelWhileCompactionIsPending(t *testing.T) {
	t.Parallel()

	policy, _ := newControllerPolicyService(t, nil)
	agent := &gatedAgentStreamer{started: make(chan struct{}), release: make(chan struct{})}
	service := newDiscussTestService(&fakeRunner{}, agent, &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{ModelID: scheduleModelID, ContextBudgetMaxTokens: 1000},
	})
	runner := newGatedCompactionRunner()
	t.Cleanup(func() { close(runner.release) })
	service.queries = policy.queries
	service.modelsService = policy.modelsService
	service.settingsService = policy.settingsService
	service.compactionService = runner

	cmd := discussCommand()
	cmd.BotID, cmd.ThreadID = scheduleBotID, scheduleSessionID
	cmd.DiscussMessages = []turn.DiscussMessage{
		{Role: "user", Content: strings.Repeat("a", 2000)},
		{Role: "assistant", Content: strings.Repeat("b", 2000)},
	}
	h, err := service.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.started:
	case <-time.After(2 * time.Second):
		t.Fatal("discuss under hard pressure never reached the model")
	}
	runner.assertNoPass(t)

	close(agent.release)
	drained := make(chan []turn.Event, 1)
	go func() { drained <- drainDiscuss(t, h) }()
	var events []turn.Event
	select {
	case events = <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("discuss run did not end")
	}
	for _, event := range events {
		if event.Kind == turn.DiscussEventRecompose {
			t.Fatal("discuss under hard pressure ended its run to recompose")
		}
	}
	cfg := runner.awaitPass(t)
	if cfg.SessionID != scheduleSessionID || !cfg.HardPressure || cfg.ContextWindowTokens != 1000 || cfg.TargetTokens != 400 {
		t.Fatalf("background pass = %+v, want a hard-pressure pass over the 1000-token budget", cfg)
	}
}

// The pass a turn schedules starts once that run ends, so the summarizer never
// competes with the turn's own model calls for the provider.
func TestScheduledCompactionStartsWhenTheRunEnds(t *testing.T) {
	t.Parallel()

	service, runner := newScheduleService(t, historyAtTokens(t, 3200))
	ctx, endRun := withRunEnd(context.Background())
	resolveWithinContext(t, ctx, service, ChatRequest{BotID: scheduleBotID, ChatID: scheduleBotID, ThreadID: scheduleSessionID, Query: "next"})
	runner.assertNoPass(t)

	endRun()
	if cfg := runner.awaitPass(t); cfg.SessionID != scheduleSessionID || !cfg.HardPressure {
		t.Fatalf("background pass = %+v, want the hard-pressure pass the turn scheduled", cfg)
	}
}
