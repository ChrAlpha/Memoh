package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// collectTurn drains a turn handle and returns its event payloads, the
// run_terminal events among them, and the turn errors.
func collectTurn(t *testing.T, handle turn.RunHandle) ([]string, []turn.RunTerminal, []error) {
	t.Helper()
	var (
		payloads  []string
		terminals []turn.RunTerminal
		errs      []error
	)
	timeout := time.After(5 * time.Second)
	for events, errCh := handle.Events(), handle.Errs(); events != nil || errCh != nil; {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			payloads = append(payloads, string(event.Payload))
			if terminal, ok := turn.RunTerminalFrom(event); ok {
				terminals = append(terminals, terminal)
				if i := len(payloads); i > 0 && event.Seq != int64(i) {
					t.Fatalf("run_terminal seq = %d, want %d", event.Seq, i)
				}
			}
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			errs = append(errs, err)
		case <-timeout:
			t.Fatal("turn did not end")
		}
	}
	return payloads, terminals, errs
}

// A chat turn against a real runtime manager ends with run_terminal naming
// the state and code the run's record carries.
//
// The run executes against the real native runtime, so the state and code are
// the driver's reading of an ending the runtime produced, not a value the
// entry supplied.
func TestChatTurnEndsWithRunTerminalFromLedger(t *testing.T) {
	tests := []struct {
		name string
		mode directLifecycleModelMode
		want turn.RunTerminal
		// ledger is the run's durable record: state and error code.
		ledger [2]string
	}{
		{
			name:   "failed with a coded error",
			mode:   directLifecycleModelFailure,
			want:   turn.RunTerminal{Type: turn.EventRunTerminal, State: turn.RunStateFailed, ErrorCode: "agent.provider_request_rejected"},
			ledger: [2]string{"failed", "agent.provider_request_rejected"},
		},
		{
			name:   "completed",
			mode:   directLifecycleModelSuccess,
			want:   turn.RunTerminal{Type: turn.EventRunTerminal, State: turn.RunStateCompleted},
			ledger: [2]string{"completed", ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs := sessionledger.New()
			manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
				OwnerID: "owner-run-terminal", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: abortAlignmentFence{},
			})
			t.Cleanup(func() { _ = manager.Close() })
			fixture := newChatTurnFixture(t, tt.mode)
			service := fixture.fixture.service
			// The run is admitted against the real manager, so its ledger is
			// what the run_terminal is read against.
			bindRuntime(service, manager)
			cmd := turn.StartTurnCommand{
				TeamID: "team-1", Mode: turn.ModeChat, BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
				ChatID: agenttest.LifecycleBotID, Query: directLifecyclePrompt, IdempotencyKey: "msg-1",
			}
			handle, err := service.StartTurn(context.Background(), cmd)
			if err != nil {
				t.Fatalf("start turn: %v", err)
			}
			if !turn.ReportsRunTerminal(handle) {
				t.Fatal("handle does not report run_terminal")
			}
			payloads, terminals, _ := collectTurn(t, handle)
			if len(terminals) != 1 || !reflect.DeepEqual(terminals[0], tt.want) {
				t.Fatalf("run_terminal = %+v, want %+v (events %q)", terminals, tt.want, payloads)
			}
			var last struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(payloads[len(payloads)-1]), &last); err != nil || last.Type != turn.EventRunTerminal {
				t.Fatalf("last event = %s, want run_terminal", payloads[len(payloads)-1])
			}
			run, err := runs.Get(context.Background(), handle.RunID())
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if got := [2]string{string(run.State), run.ErrorCode}; got != tt.ledger {
				t.Fatalf("session_runs = %q, want %q", got, tt.ledger)
			}
		})
	}
}

// A run whose terminal write was refused, because its owner lost the run or
// the write failed, ends without run_terminal.
func TestChatTurnWithoutTerminalRecordSendsNoRunTerminal(t *testing.T) {
	for name, finishErr := range map[string]error{
		"ownership lost": sessionruntime.ErrRunOwnershipLost,
		"write failed":   errors.New("ledger down"),
	} {
		t.Run(name, func(t *testing.T) {
			service, admitter := newAdmittedTurnTestService()
			admitter.FinishErr = finishErr
			handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s", IdempotencyKey: "m"})
			if err != nil {
				t.Fatal(err)
			}
			_, terminals, errs := collectTurn(t, handle)
			if len(terminals) != 0 {
				t.Fatalf("run_terminal = %+v, want none", terminals)
			}
			if len(errs) != 1 {
				t.Fatalf("turn errors = %v, want the run error", errs)
			}
		})
	}
}

// The terminal record the finish write returns is what run_terminal carries.
func TestChatTurnRunTerminalCarriesFinishRecord(t *testing.T) {
	service, admitter := newAdmittedTurnTestService()
	admitter.Terminal = sessionruntime.TerminalRun{RunID: "run-1", State: "lost", ErrorCode: "runtime_owner_lease_expired"}
	handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s", IdempotencyKey: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, terminals, _ := collectTurn(t, handle)
	want := turn.RunTerminal{Type: turn.EventRunTerminal, State: "lost", ErrorCode: "runtime_owner_lease_expired"}
	if len(terminals) != 1 || !reflect.DeepEqual(terminals[0], want) {
		t.Fatalf("run_terminal = %+v, want %+v", terminals, want)
	}
}

// A consumer that canceled the run does not get run_terminal, and the pump
// does not wait for it to read one.
func TestCanceledChatTurnSendsNoRunTerminal(t *testing.T) {
	service, admitter := newAdmittedTurnTestService()
	admitter.Terminal = sessionruntime.TerminalRun{RunID: "run-1", State: "aborted"}
	handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{TeamID: "t", Mode: turn.ModeChat, BotID: "b", ThreadID: "s", IdempotencyKey: "m"})
	if err != nil {
		t.Fatal(err)
	}
	handle.Cancel()
	start := time.Now()
	_, terminals, _ := collectTurn(t, handle)
	if len(terminals) != 0 {
		t.Fatalf("run_terminal = %+v, want none after the consumer canceled", terminals)
	}
	if elapsed := time.Since(start); elapsed >= terminalEventTimeout {
		t.Fatalf("pump took %s to close", elapsed)
	}
}
