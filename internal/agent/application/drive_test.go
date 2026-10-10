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

	"github.com/felinics/memoh/internal/agent/event"
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
	phase    runPhase
	deferred bool
	// commitErr, when set, is what the ending's history write returns.
	commitErr error
	// commitRows records the row flag each commit was called with.
	commitRows *[]bool
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
		ending.Phase = phaseExecution
	}
	ending.Commit = func(_ context.Context, _ *RunFailure, row bool) (commitSummary, error) {
		if e.commitRows != nil {
			*e.commitRows = append(*e.commitRows, row)
		}
		return commitSummary{}, e.commitErr
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

func newDriveService(pub *recordingTurnEventPublisher, admitter turnAdmitter) *Service {
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
		Input:      inputSynthetic,
		Unattended: true,
		Setup: func(context.Context) (runPlan, error) {
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != endCompleted {
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	spec := scheduledSpec(exec, &recordingDelivery{})
	spec.Setup = func(context.Context) (runPlan, error) {
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
	ending.Commit = func(ctx context.Context, f *RunFailure, row bool) (commitSummary, error) {
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})

	result := svc.drive(context.Background(), scheduledSpec(exec, &recordingDelivery{}))

	if result.End != endFailed {
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != endFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Code != apperror.CodeSessionPublishFailed {
		t.Fatalf("failure code = %q, want %s", result.Failure.Code, apperror.CodeSessionPublishFailed)
	}
	if result.Failure.Cause == nil || !errors.Is(result.Failure.Cause, errRunStopped) {
		if errors.Is(result.Failure.Cause, errRunStopped) {
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
	admitter := &scriptedAdmitter{started: true}
	svc := newDriveService(pub, admitter)
	delivery := &recordingDelivery{}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != endOwnershipLost {
		t.Fatalf("end = %s, want ownership_lost", result.End)
	}
	for _, ev := range pub.published() {
		if ev.IsTerminal() {
			t.Fatalf("published %#v after ownership loss", ev)
		}
	}
	if len(admitter.finishes) != 0 {
		t.Fatalf("terminal writes = %#v, want none after ownership loss", admitter.finishes)
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	delivery := &recordingDelivery{}

	stop := make(chan struct{})
	spec := scheduledSpec(exec, delivery)
	spec.StopCh = stop
	close(stop)

	result := svc.drive(context.Background(), spec)

	if result.End != endStopped {
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
		facts      endingFacts
		wantEnd    runEnd
		wantCode   apperror.Code
		input      inputOwnership
		hook       bool
		replacing  bool
		wantRow    bool
		stoppedCtx bool
	}{
		{
			name:    "clean terminal completes",
			facts:   endingFacts{Phase: phaseExecution, Terminal: &terminalCompleted},
			wantEnd: endCompleted,
			input:   inputAccepted,
			wantRow: false,
		},
		{
			name:    "deferred parks",
			facts:   endingFacts{Phase: phaseExecution, Terminal: &terminalCompleted, Deferred: true},
			wantEnd: endParked,
			input:   inputAccepted,
			wantRow: false,
		},
		{
			name:       "stopped before the first failure",
			facts:      endingFacts{Phase: phaseExecution, Terminal: &terminalAbort, Declared: providerErr, StoppedAtDeclare: true},
			wantEnd:    endStopped,
			input:      inputAccepted,
			wantRow:    false,
			stoppedCtx: true,
		},
		{
			name:       "failed before the stop keeps its name",
			facts:      endingFacts{Phase: phaseExecution, Terminal: &terminalAbort, Declared: providerErr, StoppedAtDeclare: false},
			wantEnd:    endFailed,
			wantCode:   apperror.CodeAgentProviderAuthFailed,
			input:      inputAccepted,
			wantRow:    true,
			stoppedCtx: true,
		},
		{
			name:     "loop guard has its own code",
			facts:    endingFacts{Phase: phaseExecution, Terminal: &terminalLoop},
			wantEnd:  endFailed,
			wantCode: apperror.CodeAgentLoopDetected,
			input:    inputAccepted,
			wantRow:  true,
		},
		{
			name:     "abort without a reason fails unnamed",
			facts:    endingFacts{Phase: phaseExecution, Terminal: &terminalFailed},
			wantEnd:  endFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    inputAccepted,
			wantRow:  true,
		},
		{
			name:     "no terminal and nothing declared",
			facts:    endingFacts{Phase: phaseExecution},
			wantEnd:  endFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    inputAccepted,
			wantRow:  true,
		},
		{
			name:     "hook refusal leaves a row for a held input",
			facts:    endingFacts{Phase: phaseSetup, Declared: errors.New("hook denied")},
			wantEnd:  endFailed,
			wantCode: apperror.CodeHookUserMessageFailed,
			input:    inputHeld,
			hook:     true,
			wantRow:  true,
		},
		{
			name:     "setup failure hands a held input back",
			facts:    endingFacts{Phase: phaseSetup, Declared: errors.New("resolve failed")},
			wantEnd:  endFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    inputHeld,
			wantRow:  false,
		},
		{
			name:     "setup failure keeps an accepted input",
			facts:    endingFacts{Phase: phaseSetup, Declared: errors.New("resolve failed")},
			wantEnd:  endFailed,
			wantCode: apperror.CodeRuntimeRunFailed,
			input:    inputAccepted,
			wantRow:  true,
		},
		{
			name:     "synthetic input never writes history",
			facts:    endingFacts{Phase: phaseExecution, Declared: providerErr},
			wantEnd:  endFailed,
			wantCode: apperror.CodeAgentProviderAuthFailed,
			input:    inputSynthetic,
			wantRow:  false,
		},
		{
			name:      "a replacement turn keeps only named codes",
			facts:     endingFacts{Phase: phaseExecution, Declared: providerErr},
			wantEnd:   endFailed,
			wantCode:  apperror.CodeAgentProviderAuthFailed,
			input:     inputAccepted,
			replacing: true,
			wantRow:   true,
		},
		{
			name:      "a replacement turn drops an unnamed failure",
			facts:     endingFacts{Phase: phaseExecution, Declared: errors.New("boom")},
			wantEnd:   endFailed,
			wantCode:  apperror.CodeRuntimeRunFailed,
			input:     inputAccepted,
			replacing: true,
			wantRow:   false,
		},
		{
			name:     "a failed history write ends a parked run",
			facts:    endingFacts{Phase: phaseCommit, Terminal: &terminalCompleted, Deferred: true, Commit: errors.New("write failed")},
			wantEnd:  endFailed,
			wantCode: apperror.CodeSessionHistoryInconsistent,
			input:    inputAccepted,
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
				cancel(errRunStopped)
				defer cancel(nil)
			}
			facts := tc.facts
			facts.RunCtx = ctx
			facts.Hook = tc.hook
			end, failure := nameRunEnd(facts)
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
			row := wantsFailureRow(tc.input, end, failure, tc.hook, tc.replacing)
			if row != tc.wantRow {
				t.Fatalf("wantsFailureRow = %v, want %v", row, tc.wantRow)
			}
		})
	}
}

// wantsFailureRow over every ownership and phase combination, so a table that
// grew a case cannot silently change another.
func TestWantsFailureRowTable(t *testing.T) {
	t.Parallel()

	phases := []runPhase{phaseSetup, phaseExecution, phaseCommit, phasePublish}
	owned := []inputOwnership{inputHeld, inputAccepted, inputSynthetic}

	for _, o := range owned {
		for _, phase := range phases {
			for _, replacing := range []bool{false, true} {
				f := &RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: phase}
				got := wantsFailureRow(o, endFailed, f, false, replacing)
				want := expectedRow(o, phase, replacing)
				if got != want {
					t.Fatalf("ownership %d phase %d replacing %v: wantsFailureRow = %v, want %v",
						o, phase, replacing, got, want)
				}
			}
		}
	}
	// Only a failure leaves a row.
	if wantsFailureRow(inputAccepted, endCompleted, nil, false, false) {
		t.Fatal("a completed run left a failure row")
	}
	if wantsFailureRow(inputAccepted, endParked, nil, false, false) {
		t.Fatal("a parked run left a failure row")
	}
}

// expectedRow states the rule independently of the implementation.
func expectedRow(o inputOwnership, phase runPhase, replacing bool) bool {
	switch o {
	case inputSynthetic:
		return false
	case inputHeld:
		if replacing {
			return false
		}
		return phase == phaseExecution || phase == phasePublish
	default:
		if replacing {
			// Only codes the history whitelist names.
			return phase == phaseExecution
		}
		return phase != phaseCommit
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
	admitter := &scriptedAdmitter{started: true}
	svc := newDriveService(pub, admitter)
	delivery := &recordingDelivery{goneAt: 2}

	result := svc.drive(context.Background(), scheduledSpec(exec, delivery))

	if result.End != endCompleted {
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
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
	if result.End != endCompleted {
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	spec := scheduledSpec(exec, &recordingDelivery{})
	spec.Setup = func(context.Context) (runPlan, error) {
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
	admitter := &scriptedAdmitter{started: true}
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
	if len(admitter.finishes) != 1 {
		t.Fatalf("terminal writes = %#v, want one", admitter.finishes)
	}
	if admitter.finishes[0].args["dep_id"] != "codex" {
		t.Fatalf("terminal write args = %#v, want them carried", admitter.finishes[0].args)
	}
}

// The delivered error event never carries the cause's text: a subscriber reads
// a code and the catalog's copy, and the agent's own words stay behind it.
func TestDriveDeliveredFailureKeepsTheCausePrivate(t *testing.T) {
	t.Parallel()

	cause := errors.New("SECRET provider boom")
	exec := &scriptedExecutor{cause: cause}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})

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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	delivery := &recordingDelivery{}
	spec := scheduledSpec(nil, delivery)
	spec.Setup = func(context.Context) (runPlan, error) {
		return runPlan{}, errors.New("resolve failed")
	}

	result := svc.drive(context.Background(), spec)

	if result.End != endFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Phase != phaseSetup {
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

	messages := &recordingMessageService{}
	pub := &recordingTurnEventPublisher{}
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	svc.messageService = messages
	delivery := &recordingDelivery{}
	spec := scheduledSpec(nil, delivery)
	spec.Req = ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "hello"}
	spec.Input = inputHeld
	spec.Setup = func(context.Context) (runPlan, error) {
		return runPlan{}, apperror.Wrap(apperror.CodeHookUserMessageFailed, errors.New("policy refused"), nil)
	}

	result := svc.drive(context.Background(), spec)

	if result.Failure == nil || result.Failure.Code != apperror.CodeHookUserMessageFailed {
		t.Fatalf("failure = %#v, want the hook's code", result.Failure)
	}
	if len(messages.persisted) != 2 {
		t.Fatalf("persisted %d rows, want the user row and the failure row", len(messages.persisted))
	}
	for i, row := range messages.persisted {
		if got, _ := row.Metadata[messagepkg.HistoryFailureOriginMetadataKey].(string); got != messagepkg.HistoryFailureOriginUserMessageHook {
			t.Fatalf("row %d origin = %#v, want %q", i, row.Metadata[messagepkg.HistoryFailureOriginMetadataKey], messagepkg.HistoryFailureOriginUserMessageHook)
		}
	}
}

// A setup failure that was not the hook's refusal hands the input back instead
// of writing a row, which is what lets the composer restore the draft.
func TestDriveNonHookSetupFailureWritesNoRow(t *testing.T) {
	t.Parallel()

	messages := &recordingMessageService{}
	svc := newDriveService(&recordingTurnEventPublisher{}, &scriptedAdmitter{started: true})
	svc.messageService = messages
	spec := scheduledSpec(nil, &recordingDelivery{})
	spec.Req = ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "hello"}
	spec.Input = inputHeld
	spec.Setup = func(context.Context) (runPlan, error) {
		return runPlan{}, errors.New("resolve failed")
	}

	result := svc.drive(context.Background(), spec)

	if result.End != endFailed {
		t.Fatalf("end = %s, want failed", result.End)
	}
	if len(messages.persisted) != 0 {
		t.Fatalf("persisted %#v, want nothing for a held input the composer restores", messages.persisted)
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
	svc := newDriveService(pub, &scriptedAdmitter{started: true})
	spec := scheduledSpec(exec, &recordingDelivery{})
	spec.Input = inputAccepted

	result := svc.drive(context.Background(), spec)

	if result.End != endFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Code != apperror.CodeSessionPublishFailed {
		t.Fatalf("failure code = %q, want %s", result.Failure.Code, apperror.CodeSessionPublishFailed)
	}
	var refused publishRefused
	if !errors.As(result.Failure.Cause, &refused) {
		t.Fatalf("failure cause = %v, want it wrapped in publishRefused", result.Failure.Cause)
	}
	if errors.Is(result.Failure.Cause, errRunStopped) || errors.Is(result.Failure.Cause, context.Canceled) {
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
	svc := newDriveService(&recordingTurnEventPublisher{}, &scriptedAdmitter{started: true})
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

var (
	_ = fmt.Sprintf
	_ = messagepkg.HistoryErrorCodeMetadataKey
	_ = errs.Recorded
	_ = sdk.Usage{}
)
