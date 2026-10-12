package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

type compactingLifecycleQueries struct {
	noDecisionQueries
}

func (q compactingLifecycleQueries) GetSettingsByBotID(ctx context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	row, err := q.directLifecycleQueries.GetSettingsByBotID(ctx, botID)
	row.CompactionEnabled = true
	row.CompactionThreshold = 1000
	return row, err
}

// newRunEndFixture is a native turn over history past the compaction
// threshold whose provider call stays open until the run is canceled.
func newRunEndFixture(t *testing.T) (directLifecycleFixture, *gatedCompactionRunner) {
	t.Helper()
	fixture := newDirectLifecycleFixture(t, directLifecycleModelBlock)
	queries := compactingLifecycleQueries{noDecisionQueries{fixture.service.queries.(*directLifecycleQueries)}}
	fixture.service.queries = queries
	fixture.service.settingsService = settings.NewService(slog.New(slog.DiscardHandler), queries, nil, nil)
	fixture.service.messageService = &memoryQueryMessageService{messages: historyAtTokens(t, 2000)}
	runner := newGatedCompactionRunner()
	t.Cleanup(func() { close(runner.release) })
	fixture.service.compactionService = runner
	return fixture, runner
}

// assertPassFollowsRun checks that the pass a run schedules waits while the
// run is still inside its provider call, then starts once the run ends.
func assertPassFollowsRun(t *testing.T, fixture directLifecycleFixture, runner *gatedCompactionRunner, cancel context.CancelFunc, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	runner.assertNoPass(t)
	cancel()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled run did not end")
	}
	if cfg := runner.awaitPass(t); cfg.SessionID != lifecycleTestSessionID {
		t.Fatalf("background pass = %+v, want the run's session", cfg)
	}
}

func runEndRequest() ChatRequest {
	return ChatRequest{
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	}
}

func TestStreamChatCompactsAfterTheRunEnds(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks, errs := fixture.service.StreamChat(ctx, runEndRequest())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for range chunks {
		}
		for range errs {
		}
	}()
	assertPassFollowsRun(t, fixture, runner, cancel, ended)
}

func TestWebSocketStreamCompactsAfterTheRunEnds(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan WSStreamEvent, 64)
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for range events {
		}
	}()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = fixture.service.StreamChatWS(ctx, runEndRequest(), events, nil)
		close(events)
		<-consumed
	}()
	assertPassFollowsRun(t, fixture, runner, cancel, ended)
}

func TestScheduledTriggerCompactsAfterItsTerminalWrite(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	held := holdTerminalWrites(t, fixture.service)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = fixture.service.TriggerSchedule(ctx, lifecycleTestBotID, schedule.TriggerPayload{
			FireID:      "run-end-fire",
			SessionID:   lifecycleTestSessionID,
			Command:     directLifecyclePrompt,
			OwnerUserID: "user-1",
		}, "")
	}()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	cancel()
	held.awaitFinishing(t)
	runner.assertNoPassWithin(t, 100*time.Millisecond)
	held.releaseOnce()
	if cfg := runner.awaitPass(t); cfg.SessionID != lifecycleTestSessionID {
		t.Fatalf("background pass = %+v, want the run's session", cfg)
	}
}

// Shutdown first closes admission and then interrupts every local run. The
// pass an interrupted run deferred would claim rows and die with the process,
// keeping them out of compaction until the claim goes stale, so none starts.
func TestShutdownInterruptedRunStartsNoCompaction(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: "owner-shutdown", OwnerLeaseTTL: time.Minute, Ledger: sessionledger.New(), Fence: abortAlignmentFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	fixture.service.SetSessionRuntime(manager)

	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "shutdown-team",
		Mode:                 turn.ModeChat,
		BotID:                lifecycleTestBotID,
		ChatID:               lifecycleTestBotID,
		ThreadID:             lifecycleTestSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for range handle.Events() {
		}
		for range handle.Errs() {
		}
	}()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	if err := manager.InterruptForShutdown(context.Background()); err != nil {
		t.Fatalf("InterruptForShutdown: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted run did not end")
	}
	runner.assertNoPassWithin(t, 300*time.Millisecond)
}

// heldFinishAdmitter holds every terminal write until released.
type heldFinishAdmitter struct {
	turnAdmitter
	finishing chan struct{}
	release   chan struct{}
}

func holdTerminalWrites(t *testing.T, s *Service) heldFinishAdmitter {
	t.Helper()
	held := heldFinishAdmitter{turnAdmitter: s.sessionRuntime, finishing: make(chan struct{}, 1), release: make(chan struct{})}
	s.sessionRuntime = held
	t.Cleanup(held.releaseOnce)
	return held
}

func (a heldFinishAdmitter) FinishRunWithErrorCode(ctx context.Context, handle sessionruntime.RunHandle, status, message string) (sessionruntime.TerminalRun, error) {
	a.finishing <- struct{}{}
	<-a.release
	return a.turnAdmitter.FinishRunWithErrorCode(ctx, handle, status, message)
}

func (a heldFinishAdmitter) releaseOnce() {
	select {
	case <-a.release:
	default:
		close(a.release)
	}
}

func (a heldFinishAdmitter) awaitFinishing(t *testing.T) {
	t.Helper()
	select {
	case <-a.finishing:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not reach its terminal write")
	}
}

type agentStreamerFunc func(context.Context, native.RunConfig) <-chan native.StreamEvent

func (f agentStreamerFunc) Stream(ctx context.Context, cfg native.RunConfig) <-chan native.StreamEvent {
	return f(ctx, cfg)
}

// A pass reads only the turns of terminal runs, so the work a run defers to its
// end starts after the run's terminal write rather than when its stream closes.
// A follow-up starts from the terminal observer of the run before it, while
// that run has yet to end, and still ends on its own terminal write.
func TestRunEndFollowsTheTerminalWrite(t *testing.T) {
	chat := func(deferred chan struct{}) *Service {
		return newTurnTestService(testChatStreamerFunc(func(ctx context.Context, _ ChatRequest) (<-chan StreamChunk, <-chan error) {
			AfterRunEnds(ctx, func() { close(deferred) })
			chunks, errs := make(chan StreamChunk), make(chan error)
			close(chunks)
			close(errs)
			return chunks, errs
		}))
	}
	discuss := func(deferred chan struct{}) *Service {
		return newDiscussTestService(&fakeRunner{}, agentStreamerFunc(func(ctx context.Context, _ native.RunConfig) <-chan native.StreamEvent {
			AfterRunEnds(ctx, func() { close(deferred) })
			events := make(chan native.StreamEvent, 1)
			events <- native.StreamEvent{Type: native.EventAgentEnd, Messages: json.RawMessage(`[{"role":"assistant","content":"done"}]`)}
			close(events)
			return events
		}), &fakeDiscussService{})
	}
	startTurn := func(ctx context.Context, mode turn.Mode) func(*testing.T, *Service) {
		return func(t *testing.T, s *Service) {
			cmd := turn.StartTurnCommand{TeamID: "t", Mode: mode, BotID: "b", ThreadID: "s"}
			if mode == turn.ModeDiscuss {
				cmd = discussCommand()
			}
			h, err := s.StartTurn(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			go drainHandle(h)
		}
	}
	previousRun, _ := WithRunEnd(context.Background())
	cases := map[string]struct {
		service func(chan struct{}) *Service
		start   func(*testing.T, *Service)
	}{
		"start turn": {chat, startTurn(context.Background(), turn.ModeChat)},
		"follow-up":  {chat, startTurn(previousRun, turn.ModeChat)},
		"discuss":    {discuss, startTurn(context.Background(), turn.ModeDiscuss)},
		"session resume": {chat, func(t *testing.T, s *Service) {
			s.SetAllowedTeam("00000000-0000-0000-0000-000000000001")
			s.logger = slog.New(slog.DiscardHandler)
			raw, _ := json.Marshal(map[string]any{"resume": resumeContext{Version: 1, ChatID: "chat", UserID: "sender", Query: "task"}})
			row := sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty("00000000-0000-0000-0000-000000000001"), RunID: db.ParseUUIDOrEmpty(uuid.NewString()), BotID: db.ParseUUIDOrEmpty(uuid.NewString()), SessionID: db.ParseUUIDOrEmpty(uuid.NewString()), InputJson: raw}
			if _, err := s.resumeInterruptedSession(t.Context(), row); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			deferred := make(chan struct{})
			s := c.service(deferred)
			held := holdTerminalWrites(t, s)
			c.start(t, s)
			held.awaitFinishing(t)
			select {
			case <-deferred:
				t.Fatal("work deferred to the run's end started before its terminal write")
			case <-time.After(100 * time.Millisecond):
			}
			held.releaseOnce()
			select {
			case <-deferred:
			case <-time.After(3 * time.Second):
				t.Fatal("work deferred to the run's end never started")
			}
		})
	}
}

// The WebSocket handler writes the run's terminal state after the stream
// returns, so the run ends when that owner says so.
func TestWebSocketStreamCompactsWhenItsOwnerEndsTheRun(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, endRun := WithRunEnd(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan WSStreamEvent, 64)
	go func() {
		for range events {
		}
	}()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_, _ = fixture.service.StreamChatWS(ctx, runEndRequest(), events, nil)
		close(events)
	}()
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled stream did not return")
	}
	runner.assertNoPassWithin(t, 100*time.Millisecond)
	endRun()
	if cfg := runner.awaitPass(t); cfg.SessionID != lifecycleTestSessionID {
		t.Fatalf("background pass = %+v, want the run's session", cfg)
	}
}
