package drive_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func TestDriveCompletesAndDeliversTheTerminalLast(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "working"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed (failure %v)", result.End, result.Failure)
	}
	if failure := drive.PublicRunFailure(result.Failure); failure != nil {
		t.Fatalf("failure = %v, want none", failure)
	}
	events, ended, settled := delivery.Snapshot()
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
	published := pub.Published()
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

	terminal := agenttest.TerminalEvent(t, "done")
	var order []string
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{OnPublish: func(ev native.StreamEvent) {
		if ev.IsTerminal() {
			order = append(order, "terminal-published")
		}
	}}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	spec := drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{})
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: &markedExecutor{inner: exec, order: &order}}, nil
	}

	svc.Drive(context.Background(), spec)

	if len(order) != 2 || order[0] != "history-written" || order[1] != "terminal-published" {
		t.Fatalf("order = %v, want the history write before the terminal publication", order)
	}
}

// A history write that failed withholds the terminal: the run's durable
// outcome is whatever the terminal write records, and publishing a completion
// the history does not have is what this ordering prevents.
func TestDriveWithholdsTheTerminalWhenTheHistoryWriteFails(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "must not look complete")
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal, CommitErr: errors.New("history unavailable")}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	if result.End != outcome.EndFailed {
		t.Fatalf("end = %s, want failed", result.End)
	}
	if result.Failure == nil || result.Failure.Code != apperror.CodeSessionHistoryInconsistent {
		t.Fatalf("failure = %#v, want %s", result.Failure, apperror.CodeSessionHistoryInconsistent)
	}
	for _, ev := range pub.Published() {
		if ev.IsTerminal() {
			t.Fatalf("published terminal event %#v after a failed history write", ev)
		}
	}
}

// A run nobody admitted publishes nothing and writes no terminal state, which
// is what keeps a test double and a discuss-era caller from projecting.
func TestDriveUnadmittedRunDeliversWithoutPublishing(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}
	spec := drivetest.ScheduledSpec(exec, delivery)
	spec.Run.Handle.FencingToken = 0

	result := svc.Drive(context.Background(), spec)

	if result.Terminal.Applied {
		t.Fatalf("terminal = %#v, want none for a run nobody admitted", result.Terminal)
	}
	if len(pub.Published()) != 0 {
		t.Fatalf("published %#v, want nothing for an unadmitted run", pub.Published())
	}
	if events, _, _ := delivery.Snapshot(); len(events) == 0 {
		t.Fatal("delivered nothing, want the events a caller would have had published")
	}
}

// A publication the owner refuses ends the run as a failure and the subscriber
// converges on the terminal write.
func TestDrivePublicationRefusalFailsTheRun(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{ErrAt: 1, Err: errors.New("projection refused")}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

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
	events, _, _ := delivery.Snapshot()
	if len(events) < 2 {
		t.Fatalf("delivered %d events, want the producer's whole sequence", len(events))
	}
}

// Ownership loss writes no history and no terminal state: another owner names
// the run's outcome.
func TestDriveOwnershipLossWritesNothing(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{ErrAt: 1, Err: sessionruntime.ErrRunOwnershipLost}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &drivetest.RecordingDelivery{}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	if result.End != outcome.EndOwnershipLost {
		t.Fatalf("end = %s, want ownership_lost", result.End)
	}
	for _, ev := range pub.Published() {
		if ev.IsTerminal() {
			t.Fatalf("published %#v after ownership loss", ev)
		}
	}
	if len(admitter.Finishes) != 0 {
		t.Fatalf("terminal writes = %#v, want none after ownership loss", admitter.Finishes)
	}
	if _, ended, _ := delivery.Snapshot(); len(ended) != 1 || ended[0].State != "" {
		t.Fatalf("Ended = %#v, want one empty state", ended)
	}
}

// A stop ends the run without a failure, and the tail still reaches the
// subscriber so it learns the run stopped.
func TestDriveStopEndsWithoutAFailure(t *testing.T) {
	t.Parallel()

	terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}

	// The entry stops the run by ending its context with the stop marker: the
	// run names the ending from that, not from a channel of its own.
	runCtx, stop := context.WithCancelCause(context.Background())
	stop(outcome.ErrRunStopped)

	result := svc.Drive(runCtx, drivetest.ScheduledSpec(exec, delivery))

	if result.End != outcome.EndStopped {
		t.Fatalf("end = %s, want stopped (failure %#v)", result.End, result.Failure)
	}
	events, ended, _ := delivery.Snapshot()
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
			exec := &drivetest.ScriptedExecutor{Terminal: &terminal, StoppedErr: tc.stoppedErr, CommitRows: &commits}
			if !tc.noHook {
				exec.StoppedWrites = &writes
			}
			pub := &agenttest.EventRecorder{}
			if tc.lost {
				pub = &agenttest.EventRecorder{ErrAt: 1, Err: sessionruntime.ErrRunOwnershipLost}
			}
			svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
			delivery := &drivetest.RecordingDelivery{}
			runCtx, stop := context.WithCancelCause(context.Background())
			if tc.stopped {
				stop(outcome.ErrRunStopped)
			}
			defer stop(nil)

			result := svc.Drive(runCtx, drivetest.ScheduledSpec(exec, delivery))

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
			events, ended, _ := delivery.Snapshot()
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

// The delivery end going away stops delivery, not the run: the subscriber that
// left must not take a schedule fire with it.
func TestDriveGoesOnAfterTheDeliveryEndLeaves(t *testing.T) {
	t.Parallel()

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &drivetest.RecordingDelivery{GoneAt: 2}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want the run to finish with its caller gone", result.End)
	}
	events, ended, _ := delivery.Snapshot()
	if len(events) != 1 {
		t.Fatalf("delivered %d events after the caller left, want exactly the first", len(events))
	}
	if len(ended) != 1 {
		t.Fatalf("Ended calls = %d, want the run's ending reported once", len(ended))
	}
	// The terminal still reaches the projection: the run's subscribers are not
	// only the caller that just left.
	var sawTerminal bool
	for _, ev := range pub.Published() {
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

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}},
		Terminal: &terminal,
	}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	release := make(chan struct{})
	delivery := &drivetest.RecordingDelivery{Release: release}

	done := make(chan drive.RunResult, 1)
	go func() { done <- svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery)) }()

	// Publication must have happened while the delivery end is still blocked.
	deadline := time.After(2 * time.Second)
	for len(pub.Published()) < 1 {
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

	terminal := agenttest.TerminalEvent(t, "done")
	cleaned := false
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{ErrAt: 1, Err: sessionruntime.ErrRunOwnershipLost}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	spec := drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{})
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: &cleanupExecutor{inner: exec, cleaned: &cleaned}}, nil
	}

	svc.Drive(context.Background(), spec)

	if !cleaned {
		t.Fatal("cleanup was skipped after ownership loss")
	}
}

// The failure payload travels the whole way: the event the subscriber gets,
// the ending the caller sees, and the terminal record all name the same code
// and args.
func TestDriveCarriesFailureArgsToEveryConsumer(t *testing.T) {
	t.Parallel()

	args := map[string]string{"dep_id": "codex", "install_task_id": "task-1"}
	cause := apperror.Wrap(apperror.CodeAgentDependencyMissing, errors.New("missing"), args)
	exec := &drivetest.ScriptedExecutor{Cause: cause}
	pub := &agenttest.EventRecorder{}
	admitter := &agenttest.ScriptedAdmitter{Started: true}
	svc := newDriveService(pub, admitter)
	delivery := &drivetest.RecordingDelivery{}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	if result.Failure == nil || result.Failure.Code != apperror.CodeAgentDependencyMissing {
		t.Fatalf("failure = %#v, want the dependency's code", result.Failure)
	}
	if got := result.Failure.Args["dep_id"]; got != "codex" {
		t.Fatalf("failure args = %#v, want the catalog args carried", result.Failure.Args)
	}
	events, ended, _ := delivery.Snapshot()
	var failureEvent *drive.RunEvent
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
	exec := &drivetest.ScriptedExecutor{Cause: cause}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	if !errors.Is(apperror.CauseOf(result.Failure.Public()), cause) {
		t.Fatalf("failure = %v, want the private cause kept for the result record", result.Failure.Public())
	}
	events, _, _ := delivery.Snapshot()
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
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	var published *native.StreamEvent
	for _, ev := range pub.Published() {
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

	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}
	spec := drivetest.ScheduledSpec(nil, delivery)
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{}, errors.New("resolve failed")
	}

	result := svc.Drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Phase != outcome.PhaseSetup {
		t.Fatalf("failure phase = %d, want the setup phase", result.Failure.Phase)
	}
	if _, ended, _ := delivery.Snapshot(); len(ended) != 1 {
		t.Fatalf("Ended = %#v, want the run's ending reported once", ended)
	}
}

// A setup failure that the user-message hook refused leaves the two rows a
// Web composer needs to show why, and the row carries the origin so history
// preparation keeps it out of the model's context.
func TestDriveHookRefusalLeavesAMarkedFailureRow(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	pub := &agenttest.EventRecorder{}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true}, withMessages(messages))
	delivery := &drivetest.RecordingDelivery{}
	spec := drivetest.ScheduledSpec(nil, delivery)
	spec.Req = runreq.ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "hello"}
	spec.Input = outcome.InputHeld
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{}, apperror.Wrap(apperror.CodeHookUserMessageFailed, errors.New("policy refused"), nil)
	}

	result := svc.Drive(context.Background(), spec)

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
	svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withMessages(messages))
	spec := drivetest.ScheduledSpec(nil, &drivetest.RecordingDelivery{})
	spec.Req = runreq.ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "hello"}
	spec.Input = outcome.InputHeld
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{}, errors.New("resolve failed")
	}

	result := svc.Drive(context.Background(), spec)

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
	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:     []native.StreamEvent{{Type: native.EventTextDelta, Delta: "a"}, {Type: native.EventTextDelta, Delta: "b"}},
		Terminal:   &terminal,
		CommitRows: &rows,
	}
	pub := &agenttest.EventRecorder{ErrAt: 1, Err: errors.New("projection refused")}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true})
	spec := drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{})
	spec.Input = outcome.InputAccepted

	result := svc.Drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure", result)
	}
	if result.Failure.Code != apperror.CodeSessionPublishFailed {
		t.Fatalf("failure code = %q, want %s", result.Failure.Code, apperror.CodeSessionPublishFailed)
	}
	var refused drive.PublishRefused
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

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "hello"}},
		Terminal: &terminal,
	}
	svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

	events, _, _ := delivery.Snapshot()
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

// The driver builds the run's asset collector and must hand it to Setup: an
// entry closes over the spec it built, so a collector installed after Setup
// ran would never reach the request the run's history writes read, and every
// run's outbound assets would go unlinked with nothing failing.
func TestSetupReceivesTheRequestWithTheDriversAssetCollector(t *testing.T) {
	t.Parallel()

	var seen runreq.ChatRequest
	svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true})
	spec := drivetest.ScheduledSpec(nil, &drivetest.RecordingDelivery{})
	spec.Setup = func(_ context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		seen = req
		return drive.RunPlan{Exec: &drivetest.ScriptedExecutor{}, Req: agenttest.TriggerRequest()}, nil
	}

	svc.Drive(context.Background(), spec)

	if seen.OutboundAssetCollector == nil {
		t.Fatal("Setup received a request with no asset collector")
	}
}

// A stop cancels the execution context while the producer's already-emitted
// events are still being published. Publication must not fail for that alone:
// the run was stopped, not broken, and reporting it as a publication failure
// would name the wrong ending and record a failure for a run the user stopped.
func TestDriveStopDoesNotTurnPendingPublicationsIntoFailures(t *testing.T) {
	t.Parallel()

	pub := &ctxCancelledPublisher{}
	svc := newDriveService(nil, &agenttest.ScriptedAdmitter{Started: true}, withEvents(pub))

	release := make(chan struct{})
	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{
		Events: []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "a"},
			{Type: native.EventTextDelta, Delta: "b"},
		},
		Terminal:   &terminal,
		BlockUntil: release,
	}
	runCtx, stop := context.WithCancelCause(context.Background())
	spec := drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{})

	done := make(chan drive.RunResult, 1)
	go func() { done <- svc.Drive(runCtx, spec) }()
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

// A panic inside a run ends that run. This is a hosted service: letting the
// panic through would take the process down with every other session in it,
// and a recovered panic that skipped settlement would leave the caller's
// delivery end blocked forever, plus the consumer goroutine waiting on a
// producer that will never close its channel.
func TestDriveContainsAPanicInTheProducer(t *testing.T) {
	t.Parallel()

	svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}
	spec := drivetest.ScheduledSpec(&panickingExecutor{panicValue: "boom"}, delivery)

	result := svc.Drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure for the panicking run", result)
	}
	if result.Failure.Code != apperror.CodeInternal {
		t.Fatalf("failure code = %q, want %q", result.Failure.Code, apperror.CodeInternal)
	}
	// Settlement ran: the caller was told how the run ended.
	_, ended, _ := delivery.Snapshot()
	if len(ended) != 1 {
		t.Fatalf("Ended = %#v, want the ending reported once", ended)
	}
}

// The same containment applies to an entry's Setup, which runs before any
// executor exists.
func TestDriveContainsAPanicInSetup(t *testing.T) {
	t.Parallel()

	svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true})
	delivery := &drivetest.RecordingDelivery{}
	spec := drivetest.ScheduledSpec(nil, delivery)
	spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		panic("setup boom")
	}

	result := svc.Drive(context.Background(), spec)

	if result.End != outcome.EndFailed || result.Failure == nil {
		t.Fatalf("result = %#v, want a named failure for a panicking setup", result)
	}
	if result.Failure.Code != apperror.CodeInternal {
		t.Fatalf("failure code = %q, want %q", result.Failure.Code, apperror.CodeInternal)
	}
	if _, ended, _ := delivery.Snapshot(); len(ended) != 1 {
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
			svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withMessages(messages))
			spec := drivetest.ScheduledSpec(nil, &drivetest.RecordingDelivery{})
			spec.Req = runreq.ChatRequest{
				BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1",
				Query: drivePrompt, UserVisibleText: drivePrompt,
			}
			spec.Input = tc.input
			spec.Setup = func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
				return drive.RunPlan{}, apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)
			}

			result := svc.Drive(context.Background(), spec)

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

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	pub := &agenttest.EventRecorder{}
	messages := &linkingMessageService{
		assistant: messagepkg.Message{ID: "assistant-1", SessionID: "session-1", Role: "assistant", Content: []byte("hi")},
	}
	svc := newDriveService(pub, &agenttest.ScriptedAdmitter{Started: true}, withMessages(messages))
	delivery := &reportedAssetsDelivery{}
	delivery.assets = []runreq.OutboundAssetRef{{ContentHash: "hash:voice", Role: "attachment"}}

	svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, delivery))

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

	terminal := agenttest.TerminalEvent(t, "done")
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal}
	messages := &linkingMessageService{
		assistant: messagepkg.Message{ID: "assistant-1", SessionID: "session-1", Role: "assistant", Content: []byte("hi")},
	}
	svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withMessages(messages))

	result := svc.Drive(context.Background(), drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

	if result.End != outcome.EndCompleted {
		t.Fatalf("end = %s, want completed", result.End)
	}
	if len(messages.linked) != 0 {
		t.Fatalf("linked %#v, want nothing", messages.linked)
	}
}

type markedExecutor struct {
	inner *drivetest.ScriptedExecutor
	order *[]string
}

func (m *markedExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	ending := m.inner.Execute(ctx, emit)
	commit := ending.Commit
	ending.Commit = func(ctx context.Context, f *outcome.RunFailure, row bool) (drive.CommitSummary, error) {
		*m.order = append(*m.order, "history-written")
		return commit(ctx, f, row)
	}
	return ending
}

type cleanupExecutor struct {
	inner   *drivetest.ScriptedExecutor
	cleaned *bool
}

func (c *cleanupExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	ending := c.inner.Execute(ctx, emit)
	ending.Cleanup = func(context.Context) { *c.cleaned = true }
	return ending
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

func (p *ctxCancelledPublisher) HandleAgentEvent(ctx context.Context, _ sessionruntime.RunHandle, event native.StreamEvent) ([]chatview.UIMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		p.errs = append(p.errs, err)
		return nil, err
	}
	p.events = append(p.events, event)
	return nil, nil
}

func (p *ctxCancelledPublisher) refusals() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.errs)
}

// panickingExecutor is a runtime whose producer panics.
type panickingExecutor struct{ panicValue any }

func (e *panickingExecutor) Execute(_ context.Context, _ func(native.StreamEvent)) drive.ExecEnding {
	panic(e.panicValue)
}

// reportedAssetsDelivery is a caller whose outbound path collected assets.
type reportedAssetsDelivery struct {
	drivetest.RecordingDelivery
	assets []runreq.OutboundAssetRef
}

func (d *reportedAssetsDelivery) ReportedAssets() []runreq.OutboundAssetRef {
	return append([]runreq.OutboundAssetRef(nil), d.assets...)
}

func (d *reportedAssetsDelivery) Deliver(ctx context.Context, ev drive.RunEvent) bool {
	return d.RecordingDelivery.Deliver(ctx, ev)
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

// The first failure a run declares names it. A provider failure the caller was
// already shown keeps its code when a later history write also fails, whether
// the executor reports that write as its own cause or the ending's commit
// returns it.
func TestDriveDeliveredFailureKeepsItsCodeAfterALaterHistoryFailure(t *testing.T) {
	t.Parallel()

	delivered := apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("401"), nil)
	cases := []struct {
		name string
		exec func(terminal *native.StreamEvent) *drivetest.ScriptedExecutor
	}{
		{"executor reports the failed write as its cause", func(terminal *native.StreamEvent) *drivetest.ScriptedExecutor {
			return &drivetest.ScriptedExecutor{
				Events:   []native.StreamEvent{{Type: native.EventError, Code: string(apperror.CodeAgentProviderAuthFailed), Cause: delivered}},
				Terminal: terminal,
				Cause:    errors.New("commit agent step: boom"),
				Phase:    outcome.PhaseCommit,
			}
		}},
		{"the ending's commit fails", func(terminal *native.StreamEvent) *drivetest.ScriptedExecutor {
			return &drivetest.ScriptedExecutor{
				Events:    []native.StreamEvent{{Type: native.EventError, Code: string(apperror.CodeAgentProviderAuthFailed), Cause: delivered}},
				Terminal:  terminal,
				CommitErr: errors.New("write history: boom"),
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndFailed}
			admitter := &agenttest.ScriptedAdmitter{Started: true}
			svc := newDriveService(&agenttest.EventRecorder{}, admitter)
			delivery := &drivetest.RecordingDelivery{}

			result := svc.Drive(context.Background(), drivetest.ScheduledSpec(tc.exec(&terminal), delivery))

			want := string(apperror.CodeAgentProviderAuthFailed)
			if result.End != outcome.EndFailed || result.Failure == nil || string(result.Failure.Code) != want {
				t.Fatalf("result = %s / %#v, want failed with %s", result.End, result.Failure, want)
			}
			if len(admitter.Finishes) != 1 || admitter.Finishes[0].Message != want {
				t.Fatalf("terminal writes = %#v, want one recording %s", admitter.Finishes, want)
			}
			events, ended, _ := delivery.Snapshot()
			if len(ended) != 1 || ended[0].ErrorCode != want {
				t.Fatalf("Ended = %#v, want %s", ended, want)
			}
			failures := 0
			for _, ev := range events {
				if ev.Event.Type == native.EventError {
					failures++
					if ev.Event.Code != want {
						t.Fatalf("delivered failure code = %q, want %s", ev.Event.Code, want)
					}
				}
			}
			if failures != 1 {
				t.Fatalf("delivered %d failure events, want the one the stream raised", failures)
			}
		})
	}
}
