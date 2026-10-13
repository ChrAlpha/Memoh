package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

func runEndManager(t *testing.T, s *Service, runs *sessionledger.Store) *sessionruntime.Manager {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: "run-end-owner", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: abortAlignmentFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	s.SetSessionRuntime(manager)
	return manager
}

func startRunEndTurn(t *testing.T, s *Service) (turn.RunHandle, <-chan struct{}) {
	t.Helper()
	handle, err := s.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: "run-end-team", Mode: turn.ModeChat,
		BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID,
		Query: directLifecyclePrompt, UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); drainHandle(handle) }()
	return handle, done
}

func awaitRunEndProducer(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("turn producer did not finish")
	}
}

func TestCompactionWaitsForRetriedTerminalWrite(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	runs := sessionledger.New()
	runs.SetFinalizeErr(errors.New("terminal write unavailable"))
	runEndManager(t, fixture.service, runs)
	handle, done := startRunEndTurn(t, fixture.service)
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider call did not start")
	}
	handle.Cancel()
	awaitRunEndProducer(t, done)
	if state := runs.State(handle.RunID()); state != ledger.StateFinishing {
		t.Fatalf("durable state = %s, want finishing while the write fails", state)
	}
	runner.assertNoPassWithin(t, 200*time.Millisecond)
	runs.SetFinalizeErr(nil)
	runner.awaitPass(t)
	if state := runs.State(handle.RunID()); !state.Terminal() {
		t.Fatalf("pass started before terminal retry committed: %s", state)
	}
}

func TestCompactionWaitsWhileRunIsParkedForDecision(t *testing.T) {
	fixture, runner := newRunEndFixture(t)
	runs := sessionledger.New()
	manager := runEndManager(t, fixture.service, runs)
	var run sessionruntime.RunHandle
	fixture.service.turnHooks = &turnRuntimeHooks{streamChat: func(ctx context.Context, req ChatRequest) (<-chan StreamChunk, <-chan error) {
		run = req.RunHandle
		fixture.service.scheduleCompaction(ctx, req, 2000, 128000, directLifecycleModelID)
		if _, err := manager.HandleAgentEvent(ctx, run, native.StreamEvent{
			Type: native.EventToolApprovalRequest, ToolName: "exec", ToolCallID: "call-waiting",
			ApprovalID: "approval-waiting", Status: "pending",
		}); err != nil {
			t.Fatal(err)
		}
		chunks, errs := make(chan StreamChunk), make(chan error)
		close(chunks)
		close(errs)
		return chunks, errs
	}}
	handle, done := startRunEndTurn(t, fixture.service)
	awaitRunEndProducer(t, done)
	if state := runs.State(handle.RunID()); state != ledger.StateWaitingDecision {
		t.Fatalf("durable state = %s, want waiting_decision", state)
	}
	runner.assertNoPassWithin(t, 200*time.Millisecond)
	if _, err := manager.FinishRun(context.Background(), run, sessionruntime.RunStatusAborted); err != nil {
		t.Fatal(err)
	}
	runner.awaitPass(t)
	if state := runs.State(handle.RunID()); state != ledger.StateAborted {
		t.Fatalf("pass started before parked run became terminal: %s", state)
	}
}
