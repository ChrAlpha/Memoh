package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

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

func TestScheduledTriggerCompactsAfterTheRunEnds(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = fixture.service.TriggerSchedule(ctx, lifecycleTestBotID, schedule.TriggerPayload{
			FireID:      "run-end-fire",
			SessionID:   lifecycleTestSessionID,
			Command:     directLifecyclePrompt,
			OwnerUserID: "user-1",
		}, "")
	}()
	assertPassFollowsRun(t, fixture, runner, cancel, ended)
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
	*scriptedAdmitter
	finishing chan struct{}
	release   chan struct{}
}

func (a heldFinishAdmitter) FinishRunWithErrorCode(ctx context.Context, handle sessionruntime.RunHandle, status, message string) (sessionruntime.TerminalRun, error) {
	a.finishing <- struct{}{}
	<-a.release
	return a.scriptedAdmitter.FinishRunWithErrorCode(ctx, handle, status, message)
}

// A pass reads only the turns of terminal runs, so the work a run defers to its
// end starts after the run's terminal write rather than when its stream closes.
func TestRunEndFollowsTheTerminalWrite(t *testing.T) {
	starts := map[string]func(*testing.T, *Service){
		"start turn": func(t *testing.T, s *Service) {
			h, err := s.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s"})
			if err != nil {
				t.Fatal(err)
			}
			go drainHandle(h)
		},
		"session resume": func(t *testing.T, s *Service) {
			s.SetAllowedTeam("00000000-0000-0000-0000-000000000001")
			s.logger = slog.New(slog.DiscardHandler)
			raw, _ := json.Marshal(map[string]any{"resume": resumeContext{Version: 1, ChatID: "chat", UserID: "sender", Query: "task"}})
			row := sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty("00000000-0000-0000-0000-000000000001"), RunID: db.ParseUUIDOrEmpty(uuid.NewString()), BotID: db.ParseUUIDOrEmpty(uuid.NewString()), SessionID: db.ParseUUIDOrEmpty(uuid.NewString()), InputJson: raw}
			if _, err := s.resumeInterruptedSession(t.Context(), row); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			deferred := make(chan struct{})
			s, scripted := newAdmittedTurnTestService(testChatStreamerFunc(func(ctx context.Context, _ ChatRequest) (<-chan StreamChunk, <-chan error) {
				ctx, endRun := WithRunEnd(ctx)
				AfterRunEnds(ctx, func() { close(deferred) })
				chunks, errs := make(chan StreamChunk), make(chan error)
				go func() {
					defer endRun()
					close(chunks)
					close(errs)
				}()
				return chunks, errs
			}))
			admitter := heldFinishAdmitter{scriptedAdmitter: scripted, finishing: make(chan struct{}, 1), release: make(chan struct{})}
			s.sessionRuntime = admitter
			start(t, s)
			select {
			case <-admitter.finishing:
			case <-time.After(3 * time.Second):
				close(admitter.release)
				t.Fatal("run did not reach its terminal write")
			}
			select {
			case <-deferred:
				close(admitter.release)
				t.Fatal("work deferred to the run's end started before its terminal write")
			case <-time.After(100 * time.Millisecond):
			}
			close(admitter.release)
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
