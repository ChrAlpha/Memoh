package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
)

// newTestTurnRun builds a delivery end over a run context the test owns, so a
// test can end the caller without going through a whole run.
func newTestTurnRun(ctx context.Context) (*turnRun, context.CancelCauseFunc) {
	runCtx, cancel := context.WithCancelCause(ctx)
	return newTurnRun(
		runCtx, "run-1", "team-1", "thread-1",
		make(chan turn.Event, 16), make(chan error, 1), make(chan turn.InjectMessage, 16), cancel,
	), cancel
}

// drainTurnRun reads a handle's channels to their end and returns what they
// carried.
func drainTurnRun(t *testing.T, handle turn.RunHandle) ([]turn.Event, []error) {
	t.Helper()
	var (
		events []turn.Event
		errs   []error
	)
	deadline := time.After(5 * time.Second)
	for eventCh, errCh := handle.Events(), handle.Errs(); eventCh != nil || errCh != nil; {
		select {
		case event, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			events = append(events, event)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			errs = append(errs, err)
		case <-deadline:
			t.Fatal("turn run did not finish")
		}
	}
	return events, errs
}

// The events a caller reads carry the run's identity and an ordering counter
// that starts at one and never repeats.
func TestTurnRunDeliversMonotonicSequenceWithRunIdentity(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	for i := range 3 {
		payload := json.RawMessage(`{"type":"text_delta","delta":"x"}`)
		if !handle.Deliver(context.Background(), drive.RunEvent{
			Event:   native.StreamEvent{Type: native.EventTextDelta},
			Payload: payload,
		}) {
			t.Fatalf("Deliver %d = false, want true for a live caller", i)
		}
	}
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{State: turn.RunStateCompleted})

	events, errs := drainTurnRun(t, handle)
	if len(errs) != 0 {
		t.Fatalf("turn errors = %v, want none", errs)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want the three deltas and the terminal", len(events))
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d; the port indexes events from one", i, event.Seq, i+1)
		}
		if event.RunID != "run-1" || event.TeamID != "team-1" || event.ThreadID != "thread-1" {
			t.Fatalf("event %d envelope = %+v, want the run's identity", i, event)
		}
	}
}

// run_terminal is the last event and carries the terminal record's args, which
// is what a channel process renders its failure reply from.
func TestTurnRunEndsWithRunTerminalCarryingArgs(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	handle.Deliver(context.Background(), drive.RunEvent{Event: native.StreamEvent{Type: native.EventTextDelta}})
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{
		State:     turn.RunStateFailed,
		ErrorCode: string(apperror.CodeRuntimeRunFailed),
		ErrorArgs: map[string]string{"reason": "upstream refused"},
		Failure:   outcome.RunFailure{Code: apperror.CodeRuntimeRunFailed, Args: map[string]string{"reason": "upstream refused"}}.Recorded(),
	})

	events, errs := drainTurnRun(t, handle)
	if len(errs) != 1 {
		t.Fatalf("turn errors = %d, want the run's one failure", len(errs))
	}
	if got := apperror.CodeOf(errs[0]); got != apperror.CodeRuntimeRunFailed {
		t.Fatalf("reported failure code = %q, want %q", got, apperror.CodeRuntimeRunFailed)
	}
	last := events[len(events)-1]
	if last.Kind != turn.EventRunTerminal {
		t.Fatalf("last event = %q, want %q", last.Kind, turn.EventRunTerminal)
	}
	terminal, ok := turn.RunTerminalFrom(last)
	if !ok {
		t.Fatalf("last event payload = %s, want a run_terminal", last.Payload)
	}
	if terminal.State != turn.RunStateFailed || terminal.ErrorCode != string(apperror.CodeRuntimeRunFailed) {
		t.Fatalf("run_terminal = %+v, want the failed record", terminal)
	}
	if terminal.ErrorArgs["reason"] != "upstream refused" {
		t.Fatalf("run_terminal args = %v, want the failure's args", terminal.ErrorArgs)
	}
}

// A run that ended without a durable record — parked on a decision, or lost to
// another owner — ends without run_terminal, as it did before the event
// existed.
func TestTurnRunWithoutTerminalRecordSendsNoRunTerminal(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{})

	events, errs := drainTurnRun(t, handle)
	if len(events) != 0 || len(errs) != 0 {
		t.Fatalf("events = %v errors = %v, want neither for a run with no record", events, errs)
	}
}

// A caller that ended the run it was reading is not sent a terminal for it.
// The driver's own terminal write cancels the run context, so the answer is
// recorded while the run's last event is delivered rather than read from that
// context afterwards.
func TestTurnRunSendsNoRunTerminalAfterTheCallerEndedIt(t *testing.T) {
	t.Parallel()

	handle, cancel := newTestTurnRun(context.Background())
	handle.Deliver(context.Background(), drive.RunEvent{Event: native.StreamEvent{Type: native.EventTextDelta}})
	cancel(outcome.ErrRunStopped)
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{State: turn.RunStateAborted})

	events, _ := drainTurnRun(t, handle)
	for _, event := range events {
		if event.Kind == turn.EventRunTerminal {
			t.Fatalf("run_terminal = %s, want none after the caller ended the run", event.Payload)
		}
	}
}

// Deliver reports the caller gone once the run context has ended, which is how
// the driver learns to stop delivering rather than blocking on a reader that
// left.
func TestTurnRunDeliverReportsTheCallerGone(t *testing.T) {
	t.Parallel()

	handle, cancel := newTestTurnRun(context.Background())
	cancel(outcome.ErrRunStopped)
	if handle.Deliver(context.Background(), drive.RunEvent{Event: native.StreamEvent{Type: native.EventTextDelta}}) {
		t.Fatal("Deliver = true after the caller ended the run")
	}
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{})
	drainTurnRun(t, handle)
}

// Cancelling a handle stops the run it speaks for, with the driver's own stop
// marker rather than an anonymous cancellation: the driver names the ending
// from it, and a failure the run already declared keeps its own name.
func TestTurnRunCancelStopsTheRunWithTheDriversMarker(t *testing.T) {
	t.Parallel()

	runCtx, cancelCause := context.WithCancelCause(context.Background())
	handle := newTurnRun(
		runCtx, "run-1", "team-1", "thread-1",
		make(chan turn.Event, 16), make(chan error, 1), make(chan turn.InjectMessage, 16), cancelCause,
	)
	handle.Cancel()

	if !outcome.RunStopped(runCtx) {
		t.Fatalf("run context cause = %v, want the driver's stop marker", context.Cause(runCtx))
	}
}

// A steer for a run whose execution has stopped is answered as such, so a
// caller can tell a rejected steer from an accepted one.
func TestTurnRunInjectedMessageReachesTheRunThenIsRefused(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "hurry up"}); err != nil {
		t.Fatalf("Inject() error = %v, want the message accepted", err)
	}
	select {
	case got := <-handle.inject:
		if got.Text != "hurry up" {
			t.Fatalf("injected = %+v, want the message", got)
		}
	case <-time.After(time.Second):
		t.Fatal("injected message did not reach the run")
	}

	handle.Settle()
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "late"}); !errors.Is(err, errRunAlreadyFinished) {
		t.Fatalf("Inject() after Settle = %v, want %v", err, errRunAlreadyFinished)
	}
	handle.Ended(context.Background(), drive.RunEnded{})
	drainTurnRun(t, handle)
}

// The assets a caller reports back join the run's own, so the message the run
// writes links both.
func TestTurnRunCollectsTheAssetsTheCallerReports(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	handle.AddOutboundAssets([]turn.OutboundAssetRef{{ContentHash: "h1", Role: "attachment"}})
	handle.AddOutboundAssets([]turn.OutboundAssetRef{{ContentHash: "h2", Role: "attachment"}})

	refs := handle.ReportedAssets()
	if len(refs) != 2 || refs[0].ContentHash != "h1" || refs[1].ContentHash != "h2" {
		t.Fatalf("reported assets = %+v, want both in order", refs)
	}
}

// Every run this handle speaks for was admitted, so it ends with a terminal
// record or an explicit reason it has none.
func TestTurnRunReportsRunTerminal(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	if !turn.ReportsRunTerminal(handle) {
		t.Fatal("handle does not report run_terminal")
	}
}

// A caller that stopped reading must not hold the terminal back: the run has
// ended by then, so the send gives up rather than waiting for a reader.
func TestTurnRunTerminalSendGivesUpOnAStalledCaller(t *testing.T) {
	t.Parallel()

	runCtx, cancelCause := context.WithCancelCause(context.Background())
	handle := newTurnRun(
		runCtx, "run-1", "team-1", "thread-1",
		make(chan turn.Event), make(chan error, 1), make(chan turn.InjectMessage, 16), cancelCause,
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handle.Settle()
		handle.Ended(context.Background(), drive.RunEnded{State: turn.RunStateCompleted})
	}()
	select {
	case <-done:
	case <-time.After(terminalEventTimeout + 5*time.Second):
		t.Fatal("terminal send blocked past its timeout")
	}
}

// A stop is reported to the caller as a stop: no failure, and a terminal whose
// state says the run was aborted rather than failed.
func TestTurnRunStopEndsWithoutAFailure(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	handle.Deliver(context.Background(), drive.RunEvent{Event: native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}})
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{State: turn.RunStateAborted})

	events, errs := drainTurnRun(t, handle)
	if len(errs) != 0 {
		t.Fatalf("turn errors = %v, want none for a stop", errs)
	}
	terminal, ok := turn.RunTerminalFrom(events[len(events)-1])
	if !ok || terminal.State != turn.RunStateAborted || terminal.ErrorCode != "" {
		t.Fatalf("run_terminal = %+v, want an aborted run without a code", terminal)
	}
}

// The payload a caller reads is the driver's public event, not the runtime's:
// an error event carries its code and args and never the provider's text.
func TestTurnRunDeliversThePublicPayloadOnly(t *testing.T) {
	t.Parallel()

	const privateDetail = "PRIVATE_PROVIDER_TEXT"
	handle, _ := newTestTurnRun(context.Background())
	event := outcome.FailureStreamEvent(outcome.RunFailure{
		Code:  apperror.CodeCompactionModelUnavailable,
		Args:  map[string]string{"reason": "the model never answered"},
		Cause: errors.New(privateDetail),
		Phase: outcome.PhaseExecution,
	})
	handle.Deliver(context.Background(), drive.RunEvent{Event: event, Payload: drive.MarshalRunEvent(event)})
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{})

	events, _ := drainTurnRun(t, handle)
	payload := string(events[0].Payload)
	if strings.Contains(payload, privateDetail) {
		t.Fatalf("delivered payload leaks the provider's text: %s", payload)
	}
	var delivered struct {
		Type string            `json:"type"`
		Code string            `json:"code"`
		Args map[string]string `json:"args"`
	}
	if err := json.Unmarshal(events[0].Payload, &delivered); err != nil {
		t.Fatalf("decode delivered payload: %v", err)
	}
	if delivered.Code != string(apperror.CodeCompactionModelUnavailable) || delivered.Args["reason"] != "the model never answered" {
		t.Fatalf("delivered = %+v, want the failure's code and args", delivered)
	}
}

// A caller that stopped reading before the run ended loses the terminal, but
// the run still finishes: the send gives up instead of blocking the driver.
func TestTurnRunSlowCallerKeepsTheRunGoing(t *testing.T) {
	t.Parallel()

	handle, _ := newTestTurnRun(context.Background())
	start := time.Now()
	handle.Settle()
	handle.Ended(context.Background(), drive.RunEnded{State: turn.RunStateCompleted})
	if elapsed := time.Since(start); elapsed > terminalEventTimeout {
		t.Fatalf("Ended took %s, want it bounded by the terminal send timeout", elapsed)
	}
	events, _ := drainTurnRun(t, handle)
	if len(events) != 1 || events[0].Kind != turn.EventRunTerminal {
		t.Fatalf("events = %+v, want just the terminal that fit", events)
	}
}

// The caller's channel is bounded. A caller that stopped reading while events
// kept coming leaves the run's delivery blocked on a full channel, and the
// caller's own stop must release it: Deliver reports the caller gone, and the
// run can still end and close its channels.
func TestTurnRunCancelUnblocksAFullEventBuffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runCtx, cancelCause := context.WithCancelCause(t.Context())
		defer cancelCause(nil)
		handle := newTurnRun(
			runCtx, "run-1", "team-1", "thread-1",
			make(chan turn.Event, 2), make(chan error, 1), make(chan turn.InjectMessage, 1), cancelCause,
		)

		var delivered []bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 6 {
				ok := handle.Deliver(context.Background(), drive.RunEvent{Event: native.StreamEvent{Type: native.EventTextDelta}})
				delivered = append(delivered, ok)
				if !ok {
					return
				}
			}
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Deliver returned while the buffer was full and the caller still live")
		default:
		}

		handle.Cancel()
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("Deliver is still blocked after the caller cancelled the run")
		}
		if len(delivered) != 3 || !delivered[0] || !delivered[1] || delivered[2] {
			t.Fatalf("Deliver results = %v, want two accepted into the buffer, then the caller gone", delivered)
		}
		handle.Settle()
		handle.Ended(context.Background(), drive.RunEnded{State: turn.RunStateAborted})
		events, errs := drainTurnRun(t, handle)
		if len(events) != 2 || len(errs) != 0 {
			t.Fatalf("events = %d errors = %v, want the two buffered events and closed channels", len(events), errs)
		}
	})
}
