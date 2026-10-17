package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/errs"
)

// ---------------------------------------------------------------------------
// driver table: one run, one ending, every entry
// ---------------------------------------------------------------------------

// scriptedExecutor produces a fixed sequence of events and reports a fixed
// ending, so a driver test reads as the run's shape rather than as a runtime's.
type scriptedExecutor struct {
	events   []native.StreamEvent
	terminal *native.StreamEvent
	cause    error
	phase    outcome.RunPhase
	deferred bool
	// commitErr, when set, is what the ending's history write returns.
	commitErr error
	// commitRows records the row flag each commit was called with.
	commitRows *[]bool
	// stoppedWrites, when non-nil, gives the ending a CommitStopped hook that
	// counts its calls into it; nil models an executor that has nothing left to
	// write when its run is stopped.
	stoppedWrites *int
	// stoppedErr is what that hook returns.
	stoppedErr error
	// emitErr, when set, is what execute hands back as its cause.
	emitErr error
	// blockUntil, when non-nil, holds execute open until it is closed.
	blockUntil chan struct{}
}

func (e *scriptedExecutor) execute(ctx context.Context, emit func(native.StreamEvent)) execEnding {
	if e.blockUntil != nil {
		select {
		case <-e.blockUntil:
		case <-ctx.Done():
		}
	}
	for _, ev := range e.events {
		emit(ev)
	}
	ending := execEnding{
		Terminal: e.terminal,
		Cause:    e.cause,
		Phase:    e.phase,
		Deferred: e.deferred,
	}
	if c := e.emitErr; c != nil {
		ending.Cause = c
	}
	if ending.Phase == 0 {
		ending.Phase = outcome.PhaseExecution
	}
	ending.Commit = func(_ context.Context, _ *outcome.RunFailure, row bool) (commitSummary, error) {
		if e.commitRows != nil {
			*e.commitRows = append(*e.commitRows, row)
		}
		return commitSummary{}, e.commitErr
	}
	if e.stoppedWrites != nil {
		ending.CommitStopped = func(context.Context) (commitSummary, error) {
			*e.stoppedWrites++
			return commitSummary{Text: "kept"}, e.stoppedErr
		}
	}
	return ending
}

// recordingDelivery records what the driver handed it.
type recordingDelivery struct {
	mu       sync.Mutex
	events   []RunEvent
	ended    []RunEnded
	settled  int
	goneAt   int // 1-based Deliver index after which the caller is gone
	delivers int
	// release, when non-nil, blocks each Deliver until a value arrives.
	release chan struct{}
}

func (d *recordingDelivery) Deliver(_ context.Context, ev RunEvent) bool {
	if d.release != nil {
		<-d.release
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivers++
	if d.goneAt > 0 && d.delivers >= d.goneAt {
		return false
	}
	d.events = append(d.events, ev)
	return true
}

func (d *recordingDelivery) Settle() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settled++
}

func (d *recordingDelivery) Ended(_ context.Context, end RunEnded) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ended = append(d.ended, end)
}

func (d *recordingDelivery) snapshot() ([]RunEvent, []RunEnded, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]RunEvent(nil), d.events...), append([]RunEnded(nil), d.ended...), d.settled
}

// ---------------------------------------------------------------- table cases

func newDriveService(pub *recordingTurnEventPublisher, admitter runreq.TurnAdmitter) *Service {
	svc := &Service{logger: slog.New(slog.DiscardHandler)}
	if pub != nil {
		svc.publishTurnEvent = pub.publish
	}
	if admitter != nil {
		svc.sessionRuntime = admitter
	}
	return svc
}

// scheduledSpec is the schedule entry's spec, which is the one L1 entry.
func scheduledSpec(exec runExecutor, delivery RunDelivery) runSpec {
	return runSpec{
		Run: admittedRun{
			Handle: sessionruntime.RunHandle{
				BotID: "bot-1", SessionID: "session-1", RunID: "run-1",
				TurnID: "turn-1", FencingToken: 1,
			},
			TurnID: "turn-1",
			Mode:   "schedule",
		},
		Req:        triggerStreamRequest(),
		Delivery:   delivery,
		Input:      outcome.InputSynthetic,
		Unattended: true,
		Setup: func(context.Context, runreq.ChatRequest) (runPlan, error) {
			return runPlan{Exec: exec, Req: triggerStreamRequest()}, nil
		},
	}
}

func TestDriveCompletesAndDeliversTheTerminalLast(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "working"}},
		terminal: &terminal,
	}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed (failure %v)", result.End, result.Failure)
	}
	if failure := publicRunFailure(result.Failure); failure != nil {
		t.Fatalf("failure = %v, want none", failure)
	}
	events, ended, settled := delivery.snapshot()
	if len(events) != 2 {
		t.Fatalf("delivered %d events, want the delta and the terminal", len(events))
	}
	if events[len(events)-1].Event.Type != native.EventAgentEnd {
		t.Fatalf("last delivered event = %q, want the terminal", events[len(events)-1].Event.Type)
	}
	if settled != 1 {
		t.Fatalf("Settle calls = %d, want exactly one", settled)
	}
	if len(ended) != 1 {
		t.Fatalf("Ended calls = %d, want exactly one", len(ended))
	}
	// Every published event is delivered, in publication order.
	published := pub.published()
	if len(published) != len(events) {
		t.Fatalf("published %d events, delivered %d; want the same sequence", len(published), len(events))
	}
	for i := range published {
		if published[i].Type != events[i].Event.Type {
			t.Fatalf("position %d: published %q, delivered %q", i, published[i].Type, events[i].Event.Type)
		}
	}
}

// The history write runs before the terminal is published: a subscriber must
// not see the run finish before its output is durable.
func TestDrivePublishesTheTerminalAfterTheHistoryWrite(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	var order []string
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{onPublish: func(ev native.StreamEvent) {
		if ev.IsTerminal() {
			order = append(order, "terminal-published")
		}
	}}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	spec := scheduledSpec(exec, &recordingDelivery{})
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		return runPlan{Exec: &markedExecutor{inner: exec, order: &order}}, nil
	}

	svc.drive(context.Background(), spec)

	if len(order) != 2 || order[0] != "history-written" || order[1] != "terminal-published" {
		t.Fatalf("order = %v, want the history write before the terminal publication", order)
	}
}

type markedExecutor struct {
	inner *scriptedExecutor
	order *[]string
}

func (m *markedExecutor) execute(ctx context.Context, emit func(native.StreamEvent)) execEnding {
	ending := m.inner.execute(ctx, emit)
	commit := ending.Commit
	ending.Commit = func(ctx context.Context, f *outcome.RunFailure, row bool) (commitSummary, error) {
		*m.order = append(*m.order, "history-written")
		return commit(ctx, f, row)
	}
	return ending
}

// A history write that failed withholds the terminal: the run's durable
// outcome is whatever the terminal write records, and publishing a completion
// the history does not have is what this ordering prevents.
func TestDriveWithholdsTheTerminalWhenTheHistoryWriteFails(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "must not look complete")
	exec := &scriptedExecutor{terminal: &terminal, commitErr: errors.New("history unavailable")}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})

	result := svc.drive(context.Background(), scheduledSpec(exec, &recordingDelivery{}))

	if result.End != outcome.EndFailed {
		t.Fatalf("end = %s, want failed", result.End)
	}
	if result.Failure == nil || result.Failure.Code != apperror.CodeSessionHistoryInconsistent {
		t.Fatalf("failure = %#v, want %s", result.Failure, apperror.CodeSessionHistoryInconsistent)
	}
	for _, ev := range pub.published() {
		if ev.IsTerminal() {
			t.Fatalf("published terminal event %#v after a failed history write", ev)
		}
	}
}

// A run nobody admitted publishes nothing and writes no terminal state, which
// is what keeps a test double and a discuss-era caller from projecting.
func TestDriveUnadmittedRunDeliversWithoutPublishing(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}
	spec := scheduledSpec(exec, delivery)
	spec.Run.Handle.FencingToken = 0

	result := svc.drive(context.Background(), spec)

	if result.Terminal.Applied {
		t.Fatalf("terminal = %#v, want none for a run nobody admitted", result.Terminal)
	}
	if len(pub.published()) != 0 {
		t.Fatalf("published %#v, want nothing for an unadmitted run", pub.published())
	}
	if events, _, _ := delivery.snapshot(); len(events) == 0 {
		t.Fatal("delivered nothing, want the events a caller would have had published")
	}
}

// A publication the owner refuses ends the run as a failure and the subscriber
// converges on the terminal write.
func TestDrivePublicationRefusalFailsTheRun(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
		terminal: &terminal,
	}
	pub := &recordingTurnEventPublisher{errAt: 1, err: errors.New("projection refused")}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Code != apperror.CodeSessionPublishFailed {
		t.Fatalf("failure code = %q, want %s", result.Failure.Code, apperror.CodeSessionPublishFailed)
	}
	if result.Failure.Cause == nil || !errors.Is(result.Failure.Cause, outcome.ErrRunStopped) {
		if errors.Is(result.Failure.Cause, outcome.ErrRunStopped) {
			t.Fatal("a refused publication must not read as a stop")
		}
	}
	// The producer was drained, so every event it offered reached the caller.
	events, _, _ := delivery.snapshot()
	if len(events) < 2 {
		t.Fatalf("delivered %d events, want the producer's whole sequence", len(events))
	}
}

// Ownership loss writes no history and no terminal state: another owner names
// the run's outcome.
func TestDriveOwnershipLossWritesNothing(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{errAt: 1, err: sessionruntime.ErrRunOwnershipLost}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != outcome.EndOwnershipLost {
		t.Fatalf("end = %s, want ownership_lost", result.End)
	}
	for _, ev := range pub.published() {
		if ev.IsTerminal() {
			t.Fatalf("published %#v after ownership loss", ev)
		}
	}
	if len(admitter.Finishes) != 0 {
		t.Fatalf("terminal writes = %#v, want none after ownership loss", admitter.Finishes)
	}
	if _, ended, _ := delivery.snapshot(); len(ended) != 1 || ended[0].State != "" {
		t.Fatalf("Ended = %#v, want one empty state", ended)
	}
}

// A stop ends the run without a failure, and the tail still reaches the
// subscriber so it learns the run stopped.
func TestDriveStopEndsWithoutAFailure(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}

	// The entry stops the run by ending its context with the stop marker: the
	// run names the ending from that, not from a channel of its own.
	runCtx, stop := context.WithCancelCause(context.Background())
	stop(outcome.ErrRunStopped)

	result := svc.drive(runCtx, scheduledSpec(exec, delivery))

	if result.End != outcome.EndStopped {
		t.Fatalf("end = %s, want stopped (failure %#v)", result.End, result.Failure)
	}
	events, ended, _ := delivery.snapshot()
	if len(events) == 0 {
		t.Fatal("delivered nothing, want the tail published to a detached context")
	}
	if len(ended) != 1 || ended[0].Failure != nil {
		t.Fatalf("Ended = %#v, want a stop without a failure", ended)
	}
}

// A stop keeps what the user already saw. The driver asks the executor to write
// it, for every executor alike: the history write of a completed run is not
// asked for, and neither is anything for a run another owner took. A stop whose
// write failed is still a stop: it reports no failure, and its terminal is
// published.
func TestDriveStopKeepsWhatTheUserSaw(t *testing.T) {
	t.Parallel()

	stopped := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	completed := native.StreamEvent{Type: native.EventAgentEnd}

	cases := []struct {
		name       string
		terminal   native.StreamEvent
		stopped    bool
		lost       bool
		stoppedErr error
		noHook     bool
		wantEnd    outcome.RunEnd
		wantWrites int
	}{
		{name: "stopped run writes once", terminal: stopped, stopped: true, wantEnd: outcome.EndStopped, wantWrites: 1},
		{name: "failed write leaves a stop", terminal: stopped, stopped: true, stoppedErr: errors.New("database down"), wantEnd: outcome.EndStopped, wantWrites: 1},
		{name: "executor with nothing left to write", terminal: stopped, stopped: true, noHook: true, wantEnd: outcome.EndStopped},
		{name: "completed run is not asked", terminal: completed, wantEnd: outcome.EndCompleted},
		{name: "lost run is not asked", terminal: completed, lost: true, wantEnd: outcome.EndOwnershipLost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var writes int
			var commits []bool
			terminal := tc.terminal
			exec := &scriptedExecutor{terminal: &terminal, stoppedErr: tc.stoppedErr, commitRows: &commits}
			if !tc.noHook {
				exec.stoppedWrites = &writes
			}
			pub := &recordingTurnEventPublisher{}
			if tc.lost {
				pub = &recordingTurnEventPublisher{errAt: 1, err: sessionruntime.ErrRunOwnershipLost}
			}
			svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
			delivery := &recordingDelivery{}
			runCtx, stop := context.WithCancelCause(context.Background())
			if tc.stopped {
				stop(outcome.ErrRunStopped)
			}
			defer stop(nil)

			result := svc.drive(runCtx, scheduledSpec(exec, delivery))

			if result.End != tc.wantEnd {
				t.Fatalf("end = %s, want %s (failure %#v)", result.End, tc.wantEnd, result.Failure)
			}
			if writes != tc.wantWrites {
				t.Fatalf("stopped-run writes = %d, want %d", writes, tc.wantWrites)
			}
			if tc.stopped && len(commits) != 0 {
				t.Fatalf("a stop called Commit %v, want only the stopped write", commits)
			}
			if tc.wantEnd != outcome.EndStopped {
				return
			}
			if result.Failure != nil {
				t.Fatalf("failure = %#v, a stop reports none", result.Failure)
			}
			events, ended, _ := delivery.snapshot()
			if len(ended) != 1 || ended[0].Failure != nil {
				t.Fatalf("Ended = %#v, want a stop without a failure", ended)
			}
			if len(events) == 0 || !events[len(events)-1].Event.IsTerminal() {
				t.Fatalf("delivered %#v, want the terminal published after the stopped write", events)
			}
			if tc.wantWrites > 0 && result.Summary.Text != "kept" {
				t.Fatalf("summary = %#v, want what the stopped write produced", result.Summary)
			}
		})
	}
}

// The failure policy: the outcome each ending names, and whether that ending
// leaves a failure row. One table, read by every entry.
func TestNameRunEndTable(t *testing.T) {
	t.Parallel()

	terminalCompleted := native.StreamEvent{Type: native.EventAgentEnd}
	terminalAbort := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	terminalLoop := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndLoopDetected}
	terminalFailed := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndFailed}
	providerErr := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)

	cases := []struct {
		name       string
		facts      outcome.EndingFacts
		wantEnd    outcome.RunEnd
		wantCode   apperror.Code
		input      outcome.InputOwnership
		hook       bool
		replacing  bool
		wantRow    bool
		stoppedCtx bool
	}{
		{
			name:    "clean terminal completes",
			facts:   outcome.EndingFacts{Phase: outcome.PhaseExecution, Terminal: &terminalCompleted},
			wantEnd: outcome.EndCompleted,
			input:   outcome.InputAccepted,
			wantRow: false,
		},
		{
			name:    "deferred parks",
			facts:   outcome.EndingFacts{Phase: outcome.PhaseExecution, Terminal: &terminalCompleted, Deferred: true},
			wantEnd: outcome.EndParked,
			input:   outcome.InputAccepted,
			wantRow: false,
		},
		{
			name:       "stopped before the first failure",
			facts:      outcome.EndingFacts{Phase: outcome.PhaseExecution, Terminal: &terminalAbort, Declared: providerErr, StoppedAtDeclare: true},
			wantEnd:    outcome.EndStopped,
			input:      outcome.InputAccepted,
			wantRow:    false,
			stoppedCtx: true,
		},
		{
			name:       "failed before the stop keeps its name",
			facts:      outcome.EndingFacts{Phase: outcome.PhaseExecution, Terminal: &terminalAbort, Declared: providerErr, StoppedAtDeclare: false},
			wantEnd:    outcome.EndFailed,
			wantCode:   apperror.CodeAgentProviderAuthFailed,
			input:      outcome.InputAccepted,
			wantRow:    true,
			stoppedCtx: true,
		},
		{
			name:     "loop guard has its own code",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseExecution, Terminal: &terminalLoop},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeAgentLoopDetected,
			input:    outcome.InputAccepted,
			wantRow:  true,
		},
		{
			name:     "abort without a reason fails unnamed",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseExecution, Terminal: &terminalFailed},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    outcome.InputAccepted,
			wantRow:  true,
		},
		{
			name:     "no terminal and nothing declared",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseExecution},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    outcome.InputAccepted,
			wantRow:  true,
		},
		{
			name:     "hook refusal leaves a row for a held input",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseSetup, Declared: errors.New("hook denied")},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeHookUserMessageFailed,
			input:    outcome.InputHeld,
			hook:     true,
			wantRow:  true,
		},
		{
			name:     "setup failure hands a held input back",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseSetup, Declared: errors.New("resolve failed")},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    outcome.InputHeld,
			wantRow:  false,
		},
		{
			name:     "setup failure keeps an accepted input",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseSetup, Declared: errors.New("resolve failed")},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    outcome.InputAccepted,
			wantRow:  true,
		},
		{
			name:     "synthetic input never writes history",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseExecution, Declared: providerErr},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeAgentProviderAuthFailed,
			input:    outcome.InputSynthetic,
			wantRow:  false,
		},
		{
			name:      "a replacement turn keeps only named codes",
			facts:     outcome.EndingFacts{Phase: outcome.PhaseExecution, Declared: providerErr},
			wantEnd:   outcome.EndFailed,
			wantCode:  apperror.CodeAgentProviderAuthFailed,
			input:     outcome.InputAccepted,
			replacing: true,
			wantRow:   true,
		},
		{
			name:      "a replacement turn drops an unnamed failure",
			facts:     outcome.EndingFacts{Phase: outcome.PhaseExecution, Declared: errors.New("boom")},
			wantEnd:   outcome.EndFailed,
			wantCode:  apperror.CodeRuntimeRunFailed,
			input:     outcome.InputAccepted,
			replacing: true,
			wantRow:   false,
		},
		{
			name:     "a failed history write ends a parked run",
			facts:    outcome.EndingFacts{Phase: outcome.PhaseCommit, Terminal: &terminalCompleted, Deferred: true, Commit: errors.New("write failed")},
			wantEnd:  outcome.EndFailed,
			wantCode: apperror.CodeSessionHistoryInconsistent,
			input:    outcome.InputAccepted,
			wantRow:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tc.stoppedCtx {
				var cancel context.CancelCauseFunc
				ctx, cancel = context.WithCancelCause(context.Background())
				cancel(outcome.ErrRunStopped)
				defer cancel(nil)
			}
			facts := tc.facts
			facts.RunCtx = ctx
			facts.Hook = tc.hook
			end, failure := outcome.NameRunEnd(facts)
			if end != tc.wantEnd {
				t.Fatalf("end = %s, want %s", end, tc.wantEnd)
			}
			if tc.wantCode != "" {
				if failure == nil {
					t.Fatalf("failure = nil, want %s", tc.wantCode)
				}
				if failure.Code != tc.wantCode {
					t.Fatalf("code = %q, want %q", failure.Code, tc.wantCode)
				}
			}
			row := outcome.WantsFailureRow(tc.input, end, failure, tc.hook, tc.replacing)
			if row != tc.wantRow {
				t.Fatalf("wantsFailureRow = %v, want %v", row, tc.wantRow)
			}
		})
	}
}

// outcome.WantsFailureRow over every ownership and phase combination, so a table that
// grew a case cannot silently change another.
func TestWantsFailureRowTable(t *testing.T) {
	t.Parallel()

	phases := []outcome.RunPhase{outcome.PhaseSetup, outcome.PhaseExecution, outcome.PhaseCommit, outcome.PhasePublish}
	owned := []outcome.InputOwnership{outcome.InputHeld, outcome.InputAccepted, outcome.InputSynthetic}

	for _, o := range owned {
		for _, phase := range phases {
			for _, replacing := range []bool{false, true} {
				f := &outcome.RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: phase}
				got := outcome.WantsFailureRow(o, outcome.EndFailed, f, false, replacing)
				want := expectedRow(o, phase, replacing)
				if got != want {
					t.Fatalf("ownership %d phase %d replacing %v: wantsFailureRow = %v, want %v",
						o, phase, replacing, got, want)
				}
			}
		}
	}
	// Only a failure leaves a row.
	if outcome.WantsFailureRow(outcome.InputAccepted, outcome.EndCompleted, nil, false, false) {
		t.Fatal("a completed run left a failure row")
	}
	if outcome.WantsFailureRow(outcome.InputAccepted, outcome.EndParked, nil, false, false) {
		t.Fatal("a parked run left a failure row")
	}
}

// expectedRow states the rule independently of the implementation.
func expectedRow(o outcome.InputOwnership, phase outcome.RunPhase, replacing bool) bool {
	switch o {
	case outcome.InputSynthetic:
		return false
	case outcome.InputHeld:
		if replacing {
			return false
		}
		return phase == outcome.PhaseExecution || phase == outcome.PhasePublish
	default:
		if replacing {
			// Only codes the history whitelist names.
			return phase == outcome.PhaseExecution
		}
		return phase != outcome.PhaseCommit
	}
}

// The delivery end going away stops delivery, not the run: the subscriber that
// left must not take a schedule fire with it.
func TestDriveGoesOnAfterTheDeliveryEndLeaves(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
		terminal: &terminal,
	}
	pub := &recordingTurnEventPublisher{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &recordingDelivery{goneAt: 2}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want the run to finish with its caller gone", result.End)
	}
	events, ended, _ := delivery.snapshot()
	if len(events) != 1 {
		t.Fatalf("delivered %d events after the caller left, want exactly the first", len(events))
	}
	if len(ended) != 1 {
		t.Fatalf("Ended calls = %d, want the run's ending reported once", len(ended))
	}
	// The terminal still reaches the projection: the run's subscribers are not
	// only the caller that just left.
	var sawTerminal bool
	for _, ev := range pub.published() {
		if ev.IsTerminal() {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Fatal("the terminal was not published after the caller left")
	}
}

// A slow caller does not hold up publication: the step commit barrier the
// engine waits on is downstream of publication, not of delivery.
func TestDriveSlowDeliveryDoesNotBlockPublication(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}},
		terminal: &terminal,
	}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	release := make(chan struct{})
	delivery := &recordingDelivery{release: release}

	done := make(chan runResult, 1)
	go func() { done <- svc.drive(context.Background(), scheduledSpec(exec, delivery)) }()

	// Publication must have happened while the delivery end is still blocked.
	deadline := time.After(2 * time.Second)
	for len(pub.published()) < 1 {
		select {
		case <-deadline:
			close(release)
			<-done
			t.Fatal("publication waited on the delivery end")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	result := <-done
	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed", result.End)
	}
}

// A run that ended without ownership still cleans up: the teardown must not
// depend on the run being this process's to finish.
func TestDriveCleanupRunsEvenAfterOwnershipLoss(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	cleaned := false
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{errAt: 1, err: sessionruntime.ErrRunOwnershipLost}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	spec := scheduledSpec(exec, &recordingDelivery{})
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		return runPlan{Exec: &cleanupExecutor{inner: exec, cleaned: &cleaned}}, nil
	}

	svc.drive(context.Background(), spec)

	if !cleaned {
		t.Fatal("cleanup was skipped after ownership loss")
	}
}

type cleanupExecutor struct {
	inner   *scriptedExecutor
	cleaned *bool
}

func (c *cleanupExecutor) execute(ctx context.Context, emit func(native.StreamEvent)) execEnding {
	ending := c.inner.execute(ctx, emit)
	ending.Cleanup = func(context.Context) { *c.cleaned = true }
	return ending
}

// The failure payload travels the whole way: the event the subscriber gets,
// the ending the caller sees, and the terminal record all name the same code
// and args.
func TestDriveCarriesFailureArgsToEveryConsumer(t *testing.T) {
	t.Parallel()

	args := map[string]string{"dep_id": "codex", "install_task_id": "task-1"}
	cause := apperror.Wrap(apperror.CodeAgentDependencyMissing, errors.New("missing"), args)
	exec := &scriptedExecutor{cause: cause}
	pub := &recordingTurnEventPublisher{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.Failure == nil || result.Failure.Code != apperror.CodeAgentDependencyMissing {
		t.Fatalf("failure = %#v, want the dependency's code", result.Failure)
	}
	if got := result.Failure.Args["dep_id"]; got != "codex" {
		t.Fatalf("failure args = %#v, want the catalog args carried", result.Failure.Args)
	}
	events, ended, _ := delivery.snapshot()
	var failureEvent *RunEvent
	for i := range events {
		if events[i].Event.Type == native.EventError {
			failureEvent = &events[i]
		}
	}
	if failureEvent == nil {
		t.Fatal("no failure event reached the caller")
	}
	if failureEvent.Event.Args["dep_id"] != "codex" {
		t.Fatalf("delivered failure args = %#v, want them carried", failureEvent.Event.Args)
	}
	if len(ended) != 1 || ended[0].ErrorCode != string(apperror.CodeAgentDependencyMissing) {
		t.Fatalf("Ended = %#v, want the failure's code", ended)
	}
	if ended[0].ErrorArgs["dep_id"] != "codex" {
		t.Fatalf("Ended args = %#v, want them carried", ended[0].ErrorArgs)
	}
	if len(admitter.Finishes) != 1 {
		t.Fatalf("terminal writes = %#v, want one", admitter.Finishes)
	}
	if admitter.Finishes[0].Args["dep_id"] != "codex" {
		t.Fatalf("terminal write args = %#v, want them carried", admitter.Finishes[0].Args)
	}
}

// The delivered error event never carries the cause's text: a subscriber reads
// a code and the catalog's copy, and the agent's own words stay behind it.
func TestDriveDeliveredFailureKeepsTheCausePrivate(t *testing.T) {
	t.Parallel()

	cause := errors.New("SECRET provider boom")
	exec := &scriptedExecutor{cause: cause}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if !errors.Is(apperror.CauseOf(result.Failure.Public()), cause) {
		t.Fatalf("failure = %v, want the private cause kept for the result record", result.Failure.Public())
	}
	events, _, _ := delivery.snapshot()
	for _, ev := range events {
		if ev.Event.Type != native.EventError {
			continue
		}
		if strings.Contains(ev.Event.Error, "SECRET") {
			t.Fatalf("delivered failure leaked the private diagnostic: %q", ev.Event.Error)
		}
		if strings.Contains(string(ev.Payload), "SECRET") {
			t.Fatalf("delivered payload leaked the private diagnostic: %s", ev.Payload)
		}
	}
}

// The terminal event the driver publishes is owner-named, which is what stops
// the session runtime from re-deriving a stop as an error.
func TestDriveTerminalIsOwnerNamed(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})

	svc.drive(context.Background(), scheduledSpec(exec, &recordingDelivery{}))

	var published *native.StreamEvent
	for _, ev := range pub.published() {
		if ev.IsTerminal() {
			published = &ev
		}
	}
	if published == nil {
		t.Fatal("no terminal event was published")
	}
	if !published.OwnerNamed {
		t.Fatal("published terminal is not owner-named; the session runtime would re-derive its outcome")
	}
}

// Setup failure is a setup failure: a run whose Setup could not resolve its
// executor reports under the setup phase, and a synthetic input leaves no row.
func TestDriveSetupFailureNamesTheSetupPhase(t *testing.T) {
	t.Parallel()

	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}
	spec := scheduledSpec(nil, delivery)
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		return runPlan{}, errors.New("resolve failed")
	}

	result := svc.drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Phase != outcome.PhaseSetup {
		t.Fatalf("failure phase = %d, want the setup phase", result.Failure.Phase)
	}
	if _, ended, _ := delivery.snapshot(); len(ended) != 1 {
		t.Fatalf("Ended = %#v, want the run's ending reported once", ended)
	}
}

// A setup failure that the user-message hook refused leaves the two rows a
// Web composer needs to show why, and the row carries the origin so history
// preparation keeps it out of the model's context.
func TestDriveHookRefusalLeavesAMarkedFailureRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	svc.messageService = messages
	delivery := &recordingDelivery{}
	spec := scheduledSpec(nil, delivery)
	spec.Req = runreq.ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "hello"}
	spec.Input = outcome.InputHeld
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		return runPlan{}, apperror.Wrap(apperror.CodeHookUserMessageFailed, errors.New("policy refused"), nil)
	}

	result := svc.drive(context.Background(), spec)

	if result.Failure == nil || result.Failure.Code != apperror.CodeHookUserMessageFailed {
		t.Fatalf("failure = %#v, want the hook's code", result.Failure)
	}
	if len(messages.Persisted) != 2 {
		t.Fatalf("persisted %d rows, want the user row and the failure row", len(messages.Persisted))
	}
	for i, row := range messages.Persisted {
		if got, _ := row.Metadata[messagepkg.HistoryFailureOriginMetadataKey].(string); got != messagepkg.HistoryFailureOriginUserMessageHook {
			t.Fatalf("row %d origin = %#v, want %q", i, row.Metadata[messagepkg.HistoryFailureOriginMetadataKey], messagepkg.HistoryFailureOriginUserMessageHook)
		}
	}
}

// A setup failure that was not the hook's refusal hands the input back instead
// of writing a row, which is what lets the composer restore the draft.
func TestDriveNonHookSetupFailureWritesNoRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	svc.messageService = messages
	spec := scheduledSpec(nil, &recordingDelivery{})
	spec.Req = runreq.ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "hello"}
	spec.Input = outcome.InputHeld
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		return runPlan{}, errors.New("resolve failed")
	}

	result := svc.drive(context.Background(), spec)

	if result.End != outcome.EndFailed {
		t.Fatalf("end = %s, want failed", result.End)
	}
	if len(messages.Persisted) != 0 {
		t.Fatalf("persisted %#v, want nothing for a held input the composer restores", messages.Persisted)
	}
}

// A publication refused with a failure that is not ownership loss cancels the
// execution with publishRefused, so the ending is a failure rather than a
// stop, and the failure row is written.
func TestDrivePublicationRefusalCancelsExecution(t *testing.T) {
	t.Parallel()

	var rows []bool
	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events:     []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
		terminal:   &terminal,
		commitRows: &rows,
	}
	pub := &recordingTurnEventPublisher{errAt: 1, err: errors.New("projection refused")}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	spec := scheduledSpec(exec, &recordingDelivery{})
	spec.Input = outcome.InputAccepted

	result := svc.drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Code != apperror.CodeSessionPublishFailed {
		t.Fatalf("failure code = %q, want %s", result.Failure.Code, apperror.CodeSessionPublishFailed)
	}
	var refused publishRefused
	if !errors.As(result.Failure.Cause, &refused) {
		t.Fatalf("failure cause = %v, want it wrapped in publishRefused", result.Failure.Cause)
	}
	if errors.Is(result.Failure.Cause, outcome.ErrRunStopped) || errors.Is(result.Failure.Cause, context.Canceled) {
		t.Fatalf("failure cause = %v, want neither the stop marker nor a bare cancellation", result.Failure.Cause)
	}
	if len(rows) != 1 || !rows[0] {
		t.Fatalf("commit row flags = %v, want the ending to write a failure row", rows)
	}
}

// The JSON a subscriber receives is the public form: the payload parses to the
// event the driver published, with no cause in it.
func TestDriveDeliveredPayloadIsThePublicEvent(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "hello"}},
		terminal: &terminal,
	}
	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}

	svc.drive(context.Background(), scheduledSpec(exec, delivery))

	events, _, _ := delivery.snapshot()
	for _, ev := range events {
		var decoded map[string]any
		if err := json.Unmarshal(ev.Payload, &decoded); err != nil {
			t.Fatalf("payload does not decode: %v (%s)", err, ev.Payload)
		}
		if decoded["type"] != string(ev.Event.Type) {
			t.Fatalf("payload type = %v, want %q", decoded["type"], ev.Event.Type)
		}
	}
}

// The observer's declared failure is the first one, whichever entry declared
// it: a later failure is a consequence, not the run's name.
func TestRunObserverKeepsTheFirstDeclaredFailure(t *testing.T) {
	t.Parallel()

	obs := newRunObserver()
	first := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)
	obs.declare(first, false)
	obs.declare(errors.New("later diagnostic"), false)

	if !errors.Is(obs.declared, first) {
		t.Fatalf("declared = %v, want the first failure to keep its name", obs.declared)
	}
}

// A clean terminal clears an earlier retryable error: a retried failure that
// recovered is not the run's outcome.
func TestRunObserverCleanTerminalClearsAnEarlierError(t *testing.T) {
	t.Parallel()

	svc := newDriveService(nil, nil)
	obs := newRunObserver()
	assets := newOutboundAssets()
	retryErr := apperror.Wrap(apperror.CodeAgentProviderOverloaded, errors.New("503"), nil)
	obs.observe(context.Background(), svc, "bot-1", assets, false, native.StreamEvent{Type: native.EventError, Cause: retryErr})
	obs.observe(context.Background(), svc, "bot-1", assets, false, native.StreamEvent{Type: native.EventAgentEnd})

	if obs.declared != nil {
		t.Fatalf("declared = %v, want a recovered retry to clear it", obs.declared)
	}
}

// L3: 续跑
//
// A decision continuation resumes from an answer that is already committed, so
// its input is accepted: every failure, from its setup on, leaves a failure
// row in the turn the decision parked, and no new user row.

// continuationRequest is the request a continuation resumes as: the turn's
// request message is already in history.
func continuationRequest() runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID: lifecycleTestBotID, ChatID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID,
		UserMessagePersisted: true, PersistedUserMessageID: "request-message",
	}
}

// A continuation whose model call the provider refuses before its first step
// ends under the provider's code everywhere the failure is reported: the
// caller's error event with the catalog detail, the run's failure, and the
// failure row. The provider's own words reach none of them.
func TestDriveDecisionContinuationNamesTheProviderFailure(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := &Service{
		agent:          native.New(native.Deps{Logger: slog.New(slog.DiscardHandler)}),
		logger:         slog.New(slog.DiscardHandler),
		messageService: messages,
	}
	req := continuationRequest()
	cfg := native.RunConfig{
		Model:    &sdk.Model{ID: "continuation-model", Provider: rejectingSpawnProvider{}, Type: sdk.ModelTypeChat},
		Messages: []sdk.Message{sdk.UserMessage("continue after the decision")},
		Identity: native.SessionContext{BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID},
	}
	eventCh := make(chan runreq.WSStreamEvent, 64)

	err := service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: req,
		Setup: func(ctx context.Context, _ runreq.ChatRequest) (runPlan, error) {
			return service.nativeContinuationPlan(ctx, req, cfg, "model-1")
		},
	}, eventCh)
	close(eventCh)
	if err != nil {
		t.Fatalf("continuation error = %v, want the failure delivered in the stream", err)
	}

	var errorEvents []native.StreamEvent
	for raw := range eventCh {
		if strings.Contains(string(raw), "SECRET") {
			t.Fatalf("delivered event leaked the cause: %s", raw)
		}
		var ev native.StreamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode delivered event %s: %v", raw, err)
		}
		if ev.Type == native.EventError {
			errorEvents = append(errorEvents, ev)
		}
	}
	definition, _ := apperror.Lookup(apperror.CodeAgentProviderAuthFailed)
	if len(errorEvents) != 1 || errorEvents[0].Code != string(apperror.CodeAgentProviderAuthFailed) || errorEvents[0].Error != definition.Detail {
		t.Fatalf("delivered error events = %#v, want one with the provider's code and detail", errorEvents)
	}
	row := failureRows(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// failureRows is the one assistant failure row a continuation left. A
// continuation writes no user row: the turn's request message is in history.
func failureRows(t *testing.T, messages *agenttest.RecordingMessages) messagepkg.PersistInput {
	t.Helper()
	if len(messages.Persisted) != 1 || messages.Persisted[0].Role != "assistant" {
		t.Fatalf("persisted %#v, want one assistant failure row", messages.Persisted)
	}
	row := messages.Persisted[0]
	if row.TurnRequestMessageID != "request-message" {
		t.Fatalf("failure row turn request = %q, want the parked turn's request message", row.TurnRequestMessageID)
	}
	return row
}

// A continuation that fails before it reaches the model, such as storing the
// answer's result, still leaves its failure row: the answer was accepted, and
// there is nowhere to hand it back to.
func TestDriveDecisionContinuationSetupFailureLeavesAFailureRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := &Service{logger: slog.New(slog.DiscardHandler), messageService: messages}

	err := service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (runPlan, error) {
			return runPlan{}, errors.New("SECRET tool result store unavailable")
		},
	}, nil)

	if apperror.CodeOf(err) != apperror.CodeRuntimeRunFailed {
		t.Fatalf("continuation error = %v, want %s for a caller without a stream", err, apperror.CodeRuntimeRunFailed)
	}
	row := failureRows(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeRuntimeRunFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeRuntimeRunFailed)
	}
}

// A continuation whose answer no longer targets the live run lost it: another
// owner names the run's outcome, so it writes no history.
func TestDriveDecisionContinuationTargetGoneWritesNothing(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := &Service{logger: slog.New(slog.DiscardHandler), messageService: messages}

	err := service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req: continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (runPlan, error) {
			return runPlan{}, sessionruntime.ErrCommandTargetNotActive
		},
	}, nil)
	if err != nil {
		t.Fatalf("continuation error = %v, want none for a run another owner holds", err)
	}
	if len(messages.Persisted) != 0 {
		t.Fatalf("persisted %#v, want nothing", messages.Persisted)
	}
}

// fixedStream is a native runtime that produces a fixed event sequence.
type fixedStream []native.StreamEvent

func (f fixedStream) Stream(context.Context, native.RunConfig) <-chan native.StreamEvent {
	ch := make(chan native.StreamEvent, len(f))
	for _, ev := range f {
		ch <- ev
	}
	close(ch)
	return ch
}

// A failed run whose terminal still carries the conversation it was given, but
// nothing the user saw, leaves one failure row: the row is the failure's, and
// the terminal's snapshot has no output of its own to add.
func TestDriveDecisionContinuationSnapshotWithoutOutputWritesOneRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := &Service{logger: slog.New(slog.DiscardHandler), messageService: messages}
	snapshot, err := json.Marshal([]sdk.Message{sdk.UserMessage("continue after the decision")})
	if err != nil {
		t.Fatal(err)
	}
	failure := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("SECRET key"), nil)
	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventError, Code: string(apperror.CodeAgentProviderAuthFailed), Cause: failure},
			{Type: native.EventAgentAbort, Messages: snapshot, EndReason: event.EndFailed},
		},
		req:     continuationRequest(),
		timing:  newReasoningTimingTracker(nil),
		service: service,
	}

	_ = service.driveUnroutedContinuation(context.Background(), decisionContinuation{
		Req:   continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (runPlan, error) { return runPlan{Exec: exec}, nil },
	}, nil)

	row := failureRows(t, messages)
	if got := row.Metadata[messagepkg.HistoryErrorCodeMetadataKey]; got != string(apperror.CodeAgentProviderAuthFailed) {
		t.Fatalf("failure row code = %#v, want %s", got, apperror.CodeAgentProviderAuthFailed)
	}
}

// The caller that streams an unrouted continuation leaving is a stop: the run
// ends without a failure and writes no row.
func TestDriveDecisionContinuationCallerLeavingIsAStop(t *testing.T) {
	t.Parallel()

	ctx, leave := context.WithCancel(context.Background())
	eventCh := make(chan runreq.WSStreamEvent)
	var commits []bool
	exec := runFuncExecutor(func(ctx context.Context, _ func(native.StreamEvent)) execEnding {
		leave()
		<-ctx.Done()
		return execEnding{
			Terminal: &native.StreamEvent{Type: native.EventAgentAbort},
			Commit: func(_ context.Context, _ *outcome.RunFailure, row bool) (commitSummary, error) {
				commits = append(commits, row)
				return commitSummary{}, nil
			},
		}
	})
	service := &Service{logger: slog.New(slog.DiscardHandler)}

	err := service.driveUnroutedContinuation(ctx, decisionContinuation{
		Req:   continuationRequest(),
		Setup: func(context.Context, runreq.ChatRequest) (runPlan, error) { return runPlan{Exec: exec}, nil },
	}, eventCh)
	if err != nil {
		t.Fatalf("continuation error = %v, want none", err)
	}
	// A stop writes no history at all, so the ending's history write is not
	// even reached.
	if len(commits) != 0 {
		t.Fatalf("history writes = %v, want none for a stop", commits)
	}
}

var (
	_ = fmt.Sprintf
	_ = messagepkg.HistoryErrorCodeMetadataKey
	_ = errs.Recorded
	_ = sdk.Usage{}
)

// The driver builds the run's asset collector and must hand it to Setup: an
// entry closes over the spec it built, so a collector installed after Setup
// ran would never reach the request the run's history writes read, and every
// run's outbound assets would go unlinked with nothing failing.
func TestSetupReceivesTheRequestWithTheDriversAssetCollector(t *testing.T) {
	t.Parallel()

	var seen runreq.ChatRequest
	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	spec := scheduledSpec(nil, &recordingDelivery{})
	spec.Setup = func(_ context.Context, req runreq.ChatRequest) (runPlan, error) {
		seen = req
		return runPlan{Exec: &scriptedExecutor{}, Req: triggerStreamRequest()}, nil
	}

	svc.drive(context.Background(), spec)

	if seen.OutboundAssetCollector == nil {
		t.Fatal("Setup received a request with no asset collector")
	}
}

// ctxCancelledPublisher models the session runtime's gate: the publication
// races the run's cancellation, so a cancelled context can refuse an event.
// The recording double ignores its context, which is why a run that publishes
// under a cancelled execution context went unnoticed.
type ctxCancelledPublisher struct {
	mu     sync.Mutex
	events []native.StreamEvent
	errs   []error
}

func (p *ctxCancelledPublisher) publish(ctx context.Context, _ sessionruntime.RunHandle, event native.StreamEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		p.errs = append(p.errs, err)
		return err
	}
	p.events = append(p.events, event)
	return nil
}

func (p *ctxCancelledPublisher) refusals() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.errs)
}

// A stop cancels the execution context while the producer's already-emitted
// events are still being published. Publication must not fail for that alone:
// the run was stopped, not broken, and reporting it as a publication failure
// would name the wrong ending and record a failure for a run the user stopped.
func TestDriveStopDoesNotTurnPendingPublicationsIntoFailures(t *testing.T) {
	t.Parallel()

	pub := &ctxCancelledPublisher{}
	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	svc.publishTurnEvent = pub.publish

	release := make(chan struct{})
	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{
		events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventTextDelta, Delta: "b"},
		},
		terminal:   &terminal,
		blockUntil: release,
	}
	runCtx, stop := context.WithCancelCause(context.Background())
	spec := scheduledSpec(exec, &recordingDelivery{})

	done := make(chan runResult, 1)
	go func() { done <- svc.drive(runCtx, spec) }()
	stop(outcome.ErrRunStopped)
	close(release)

	result := <-done

	if refusals := pub.refusals(); refusals != 0 {
		t.Fatalf("publication refused %d time(s) because the execution context was cancelled", refusals)
	}
	if result.End == outcome.EndFailed && result.Failure != nil && result.Failure.Code == apperror.CodeSessionPublishFailed {
		t.Fatalf("a stopped run ended as a publication failure: %#v", result.Failure)
	}
}

// panickingExecutor is a runtime whose producer panics.
type panickingExecutor struct{ panicValue any }

func (e *panickingExecutor) execute(_ context.Context, _ func(native.StreamEvent)) execEnding {
	panic(e.panicValue)
}

// A panic inside a run ends that run. This is a hosted service: letting the
// panic through would take the process down with every other session in it,
// and a recovered panic that skipped settlement would leave the caller's
// delivery end blocked forever, plus the consumer goroutine waiting on a
// producer that will never close its channel.
func TestDriveContainsAPanicInTheProducer(t *testing.T) {
	t.Parallel()

	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}
	spec := scheduledSpec(&panickingExecutor{panicValue: "boom"}, delivery)

	result := svc.drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure for the panicking run", result)
	}
	if result.Failure.Code != apperror.CodeInternal {
		t.Fatalf("failure code = %q, want %q", result.Failure.Code, apperror.CodeInternal)
	}
	// Settlement ran: the caller was told how the run ended.
	_, ended, _ := delivery.snapshot()
	if len(ended) != 1 {
		t.Fatalf("Ended = %#v, want the ending reported once", ended)
	}
}

// The same containment applies to an entry's Setup, which runs before any
// executor exists.
func TestDriveContainsAPanicInSetup(t *testing.T) {
	t.Parallel()

	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &recordingDelivery{}
	spec := scheduledSpec(nil, delivery)
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		panic("setup boom")
	}

	result := svc.drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure for a panicking setup", result)
	}
	if result.Failure.Code != apperror.CodeInternal {
		t.Fatalf("failure code = %q, want %q", result.Failure.Code, apperror.CodeInternal)
	}
	if _, ended, _ := delivery.snapshot(); len(ended) != 1 {
		t.Fatalf("Ended = %#v, want the ending reported once", ended)
	}
}

// L2: IM StartTurn. A chat run's input left its submitter, so a failure in
// setup leaves the user message and a failure row: the sender whose message was
// refused sees why, and the row is what the next turn reads. A Web send in the
// same position writes nothing, because its draft can still go back to the
// composer.
func TestDriveIMStartTurnSetupFailureLeavesTheUserMessageAndRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		input    outcome.InputOwnership
		wantRows int
	}{
		{name: "IM", input: outcome.InputAccepted, wantRows: 2},
		{name: "Web send", input: outcome.InputHeld, wantRows: 0},
		{name: "schedule", input: outcome.InputSynthetic, wantRows: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			messages := &agenttest.RecordingMessages{}
			svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
			svc.messageService = messages
			spec := scheduledSpec(nil, &recordingDelivery{})
			spec.Req = runreq.ChatRequest{
				BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1",
				Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
			}
			spec.Input = tc.input
			spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
				return runPlan{}, apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)
			}

			result := svc.drive(context.Background(), spec)

			if result.Failure == nil || result.Failure.Code != apperror.CodeAgentProviderAuthFailed {
				t.Fatalf("failure = %#v, want the provider's code", result.Failure)
			}
			if len(messages.Persisted) != tc.wantRows {
				t.Fatalf("persisted %d rows, want %d", len(messages.Persisted), tc.wantRows)
			}
			if tc.wantRows == 0 {
				return
			}
			if messages.Persisted[0].Role != "user" {
				t.Fatalf("first row = %#v, want the user's own message", messages.Persisted[0])
			}
			if got, _ := messages.Persisted[1].Metadata[messagepkg.HistoryErrorCodeMetadataKey].(string); got != string(apperror.CodeAgentProviderAuthFailed) {
				t.Fatalf("failure row metadata = %#v, want the provider's code", messages.Persisted[1].Metadata)
			}
		})
	}
}

// gap 1 regression: the assets the delivery end reports — the attachments a
// channel ingested, the audio it synthesized — must be linked to the reply they
// belong to. Without this they are collected and dropped, and no test notices
// because the handle still holds them.
func TestDriveLinksTheAssetsTheDeliveryEndReports(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{terminal: &terminal}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	messages := &linkingMessageService{
		assistant: messagepkg.Message{ID: "assistant-1", SessionID: "session-1", Role: "assistant", Content: []byte("hi")},
	}
	svc.messageService = messages
	delivery := &reportedAssetsDelivery{}
	delivery.assets = []runreq.OutboundAssetRef{{ContentHash: "hash:voice", Role: "attachment"}}

	svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if len(messages.linked) == 0 {
		t.Fatal("the assets the delivery end reported were never linked")
	}
	var linked bool
	for _, ref := range messages.linked[0] {
		if ref.ContentHash == "hash:voice" {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("linked %#v, want the delivery end's asset", messages.linked)
	}
}

// A delivery end that reports no assets is not linked, and the run still
// completes: the pull is optional.
func TestDriveLinksNothingWithoutReportedAssets(t *testing.T) {
	t.Parallel()

	terminal := scheduleTerminalEvent(t, "done")
	exec := &scriptedExecutor{terminal: &terminal}
	svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
	messages := &linkingMessageService{
		assistant: messagepkg.Message{ID: "assistant-1", SessionID: "session-1", Role: "assistant", Content: []byte("hi")},
	}
	svc.messageService = messages

	result := svc.drive(context.Background(), scheduledSpec(exec, &recordingDelivery{}))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed", result.End)
	}
	if len(messages.linked) != 0 {
		t.Fatalf("linked %#v, want nothing", messages.linked)
	}
}

// reportedAssetsDelivery is a caller whose outbound path collected assets.
type reportedAssetsDelivery struct {
	recordingDelivery
	assets []runreq.OutboundAssetRef
}

func (d *reportedAssetsDelivery) reportedAssets() []runreq.OutboundAssetRef {
	return append([]runreq.OutboundAssetRef(nil), d.assets...)
}

func (d *reportedAssetsDelivery) Deliver(ctx context.Context, ev RunEvent) bool {
	return d.recordingDelivery.Deliver(ctx, ev)
}

// linkingMessageService records what the run linked to the reply, and answers
// the one lookup LinkOutboundAssets makes with a written assistant row.
type linkingMessageService struct {
	agenttest.RecordingMessages
	assistant messagepkg.Message
	linked    [][]messagepkg.AssetRef
}

func (s *linkingMessageService) ListLatestBySession(context.Context, string, int32) ([]messagepkg.Message, error) {
	return []messagepkg.Message{s.assistant}, nil
}

func (s *linkingMessageService) LinkAssets(_ context.Context, _ string, assets []messagepkg.AssetRef) error {
	s.linked = append(s.linked, append([]messagepkg.AssetRef(nil), assets...))
	return nil
}

// L4: Web
//
// The Web entries decide two things the driver reads: who holds the run's
// input, and whether the run replaces a turn history already has. A send's
// draft is still the submitter's, so a failure before execution writes nothing
// and the composer can hand it back; a retry or an edit replaces a turn, so it
// writes nothing either — the turn it replaces is already there, and only a
// failure after execution started leaves it its own row, under a code the
// history whitelist names.

// webEntrySpecs builds one spec per Web entry through the constructor the
// entry itself uses, so the input ownership and the replacement under test are
// the entry's own.
func webEntrySpecs(svc *Service, admission *WebAdmission, delivery RunDelivery) map[string]runSpec {
	return map[string]runSpec{
		"send": svc.webRunSpec(admission, runreq.ChatRequest{
			BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1",
			Query: directLifecyclePrompt, UserVisibleText: directLifecyclePrompt,
		}, delivery, outcome.InputHeld),
		"retry": svc.retryRunSpec(admission, RetryLatestMessageInput{
			BotID: "bot-1", SessionID: "session-1", TargetTurnID: "old-turn",
		}, delivery),
		"edit": svc.editRunSpec(admission, EditLatestMessageInput{
			BotID: "bot-1", SessionID: "session-1", TargetTurnID: "old-turn", Text: "edited",
		}, delivery),
	}
}

func TestDriveWebEntryFailureRowsFollowInputOwnership(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		entry   string
		phase   outcome.RunPhase
		code    apperror.Code
		hook    bool
		wantRow bool
	}{
		{entry: "send", phase: outcome.PhaseSetup, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "send", phase: outcome.PhaseSetup, code: apperror.CodeHookUserMessageFailed, hook: true, wantRow: true},
		{entry: "send", phase: outcome.PhaseExecution, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "send", phase: outcome.PhasePublish, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "retry", phase: outcome.PhaseSetup, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "edit", phase: outcome.PhaseSetup, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "retry", phase: outcome.PhaseExecution, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "edit", phase: outcome.PhaseExecution, code: apperror.CodeAgentProviderAuthFailed, wantRow: true},
		{entry: "retry", phase: outcome.PhaseExecution, code: apperror.CodeInternal},
		{entry: "edit", phase: outcome.PhaseExecution, code: apperror.CodeInternal},
		{entry: "retry", phase: outcome.PhaseCommit, code: apperror.CodeAgentProviderAuthFailed},
		{entry: "edit", phase: outcome.PhaseCommit, code: apperror.CodeAgentProviderAuthFailed},
	} {
		name := tc.entry + " " + phaseName(tc.phase) + " " + string(tc.code)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			messages := &agenttest.RecordingMessages{}
			svc := newDriveService(&recordingTurnEventPublisher{}, &agenttest.ScriptedAdmitter{Started: true})
			svc.messageService = messages
			admission, err := svc.AdmitWebRun(context.Background(), WebAdmitInput{
				BotID: "bot-1", SessionID: "session-1", InvocationID: name,
				Payload: []byte(`{"kind":"` + tc.entry + `"}`),
			})
			if err != nil || admission.Duplicate {
				t.Fatalf("AdmitWebRun() = %+v, %v; want a started run", admission, err)
			}
			// The setup is replaced by the phase's failure, so what the ending
			// writes is decided by the entry's own input ownership and
			// replacement alone.
			var commits []bool
			spec := webEntrySpecs(svc, admission, &recordingDelivery{})[tc.entry]
			req := spec.Req
			spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
				if tc.hook {
					return runPlan{}, apperror.New(apperror.CodeHookUserMessageFailed, nil)
				}
				exec := &scriptedExecutor{
					terminal:   ptrTo(scheduleTerminalEvent(t, "an answer")),
					cause:      apperror.Wrap(tc.code, errors.New("SECRET cause"), nil),
					phase:      tc.phase,
					commitRows: &commits,
				}
				return runPlan{Exec: exec, Req: req}, nil
			}

			result := svc.drive(admission.ctx, spec)
			admission.stop(outcome.ErrRunStopped)
			admission.endActive()

			if result.End != outcome.EndFailed || result.Failure == nil || result.Failure.Code != tc.code {
				t.Fatalf("result = %#v, want failed %s", result, tc.code)
			}
			if tc.hook {
				// The hook's refusal is a setup failure the driver writes
				// itself: the user has to be shown why the send was refused.
				if len(messages.Persisted) != 2 || messages.Persisted[1].Metadata[messagepkg.HistoryFailureOriginMetadataKey] != messagepkg.HistoryFailureOriginUserMessageHook {
					t.Fatalf("persisted %#v, want the hook-refused pair", messages.Persisted)
				}
				return
			}
			if len(commits) != 1 {
				t.Fatalf("history writes = %v, want one", commits)
			}
			if commits[0] != tc.wantRow {
				t.Fatalf("failure row written = %v, want %v (input %d, replacing %v, phase %s)",
					commits[0], tc.wantRow, spec.Input, spec.Replacement != nil, phaseName(tc.phase))
			}
		})
	}
}
