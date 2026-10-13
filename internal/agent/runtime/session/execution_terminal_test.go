package sessionruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
)

func TestExecutionTerminalCallbackFollowsDurableFinish(t *testing.T) {
	fixture := newAdmitFixture(t)
	var calls atomic.Int32
	input := fixture.input("execution-terminal", `{"text":"hi"}`)
	var admission Admission
	input.Execution.OnTerminal = func() {
		if state := fixture.runs.State(admission.RunID); state != ledger.StateCompleted {
			t.Errorf("callback saw ledger state %q, want completed", state)
		}
		calls.Add(1)
	}
	var err error
	admission, err = fixture.manager.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("callbacks before terminal = %d, want 0", got)
	}
	for range 2 {
		if _, err := fixture.manager.FinishRun(context.Background(), admission.Handle, RunStatusCompleted); err != nil {
			t.Fatal(err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("callbacks after finish/replay = %d, want 1", got)
		}
	}
}

func TestExecutionTerminalCallbackWaitsForParkedRun(t *testing.T) {
	fixture := newAdmitFixture(t)
	var calls atomic.Int32
	input := fixture.input("execution-terminal-parked", `{"text":"hi"}`)
	input.Execution.OnTerminal = func() { calls.Add(1) }
	admission, err := fixture.manager.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.HandleAgentEvent(context.Background(), admission.Handle, native.StreamEvent{
		Type: native.EventToolApprovalRequest, ToolName: "exec", ToolCallID: "call-waiting",
		ApprovalID: "approval-waiting", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if terminal, err := fixture.manager.FinishRun(context.Background(), admission.Handle, ""); err != nil || terminal.RunID != "" {
		t.Fatalf("parked finish = %+v, %v; want no terminal", terminal, err)
	}
	if state := fixture.runs.State(admission.RunID); state != ledger.StateWaitingDecision {
		t.Fatalf("parked state = %q, want waiting_decision", state)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("callbacks while parked = %d, want 0", got)
	}
	if _, err := fixture.manager.FinishRun(context.Background(), admission.Handle, RunStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callbacks after parked run finishes = %d, want 1", got)
	}
}

func TestExecutionTerminalCallbackWaitsForDurableRetry(t *testing.T) {
	for _, phase := range []string{"prepare", "finalize", "decision finalizer"} {
		t.Run(phase, func(t *testing.T) {
			fixture := newAdmitFixture(t)
			var calls atomic.Int32
			notified := make(chan struct{}, 2)
			input := fixture.input("execution-terminal-retry-"+phase, `{"text":"hi"}`)
			var admission Admission
			input.Execution.OnTerminal = func() {
				if state := fixture.runs.State(admission.RunID); state != ledger.StateCompleted {
					t.Errorf("retry callback saw state %q, want completed", state)
				}
				calls.Add(1)
				notified <- struct{}{}
			}
			var err error
			admission, err = fixture.manager.Admit(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			transient := errors.New("injected durable finish failure")
			switch phase {
			case "prepare":
				fixture.runs.SetPrepareErr(transient)
			case "finalize":
				fixture.runs.SetFinalizeErr(transient)
			default:
				fixture.manager.SetDecisionFinalizer(func(context.Context, RunHandle) error { return transient })
			}
			if _, err := fixture.manager.FinishRun(context.Background(), admission.Handle, RunStatusCompleted); err == nil {
				t.Fatal("finish succeeded while durable write fails")
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("callbacks before durable retry = %d, want 0", got)
			}
			fixture.runs.SetPrepareErr(nil)
			fixture.runs.SetFinalizeErr(nil)
			fixture.manager.SetDecisionFinalizer(nil)
			select {
			case <-notified:
			case <-time.After(3 * time.Second):
				t.Fatal("durable retry did not notify execution")
			}
			if _, err := fixture.manager.FinishRun(context.Background(), admission.Handle, RunStatusCompleted); err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("callbacks after retry and replay = %d, want 1", got)
			}
		})
	}
}

func TestExecutionTerminalCallbackRunsOnceForConcurrentFinish(t *testing.T) {
	fixture := newAdmitFixture(t)
	var calls atomic.Int32
	input := fixture.input("execution-terminal-concurrent", `{"text":"hi"}`)
	input.Execution.OnTerminal = func() { calls.Add(1) }
	admission, err := fixture.manager.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	fixture.manager.SetDecisionFinalizer(func(context.Context, RunHandle) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	finished := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := fixture.manager.FinishRun(context.Background(), admission.Handle, RunStatusCompleted)
			finished <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("concurrent finish did not reach terminal finalizer")
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent finish did not return")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callbacks after concurrent finish = %d, want 1", got)
	}
}

func TestExecutionTerminalCallbackDropsLostOwner(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked", true: "newer terminal"}[terminal], func(t *testing.T) {
			fixture := newAdmitFixture(t)
			var calls atomic.Int32
			input := fixture.input("execution-terminal-lost", `{"text":"hi"}`)
			input.Execution.OnTerminal = func() { calls.Add(1) }
			admission, err := fixture.manager.Admit(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				fixture.runs.Mu.Lock()
				run := fixture.runs.Runs[admission.RunID]
				run.FencingToken++
				run.State = ledger.StateAborted
				fixture.runs.Mu.Unlock()
			} else {
				fixture.manager.localControlForHandle(admission.Handle).revokeOwnership(ErrRunOwnershipLost)
			}
			if _, err := fixture.manager.FinishRun(context.Background(), admission.Handle, RunStatusCompleted); !errors.Is(err, ErrRunOwnershipLost) {
				t.Fatalf("finish error = %v, want ownership lost", err)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("callbacks after ownership loss = %d, want 0", got)
			}
			if fixture.manager.localControlForHandle(admission.Handle) != nil {
				t.Fatal("lost owner retained its control and terminal callback")
			}
		})
	}
}
