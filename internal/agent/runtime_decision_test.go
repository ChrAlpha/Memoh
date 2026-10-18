package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	sessiontest "github.com/felinics/memoh/internal/testutil/sessionruntime"
)

// continuationService is a service whose decision continuations run against
// manager: the manager routes the answer, takes the run's publications, and
// records the run's ending.
func continuationService(t *testing.T, manager *sessionruntime.Manager, opts ...testOption) *Service {
	t.Helper()
	return newTestService(t, Deps{SessionRuntime: manager}, opts...)
}

// decisionCommand is the routed command that answers the decision the parked
// run of newWaitingDecisionRuntime waits on.
func decisionCommand(handle sessionruntime.RunHandle, id string) sessionruntime.Command {
	return sessionruntime.Command{
		StreamOutput: true, ID: id, TargetID: "decision-" + id, Type: sessionruntime.CommandUserInputResponse,
		BotID: handle.BotID, SessionID: handle.SessionID, RunID: handle.RunID, Generation: handle.Generation,
	}
}

// continueDecision drives one routed continuation the way the command handler
// does: on the context and handle the parked run's control hands out, with
// setup standing in for storing the answer and resolving the model run.
func continueDecision(t *testing.T, service *Service, manager *sessionruntime.Manager, command sessionruntime.Command, setup func(context.Context, runreq.ChatRequest) (drive.RunPlan, error)) {
	t.Helper()
	live := command
	live.Generation = strings.TrimSuffix(command.Generation, "-stale")
	runCtx, cancel, handle, err := manager.DecisionContinuationContext(live)
	if err != nil {
		t.Fatalf("decision continuation context: %v", err)
	}
	defer cancel()
	service.answer.DriveRouted(runCtx, command, handle, answer.Continuation{
		Req:   runreq.ChatRequest{BotID: handle.BotID, ChatID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle, UserMessagePersisted: true},
		Setup: setup,
	})
}

// scriptedRun is a setup whose run produces events in order. A terminal event
// among them ends the run, and parks it when it carries a pending decision;
// cause, when set, is the failure the run reports besides its events.
func scriptedRun(events []native.StreamEvent, cause error) func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
	exec := &drivetest.ScriptedExecutor{Cause: cause}
	for _, ev := range events {
		if ev.IsTerminal() {
			terminal := ev
			exec.Terminal = &terminal
			exec.Deferred = ev.ApprovalID != "" || ev.UserInputID != ""
			continue
		}
		exec.Events = append(exec.Events, ev)
	}
	return func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: exec}, nil
	}
}

func runStatus(t *testing.T, manager *sessionruntime.Manager) *sessionruntime.CurrentRunView {
	t.Helper()
	snapshot, err := manager.Snapshot(context.Background(), agenttest.LifecycleBotID, agenttest.LifecycleSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.CurrentRunView
}

// A continuation that reports its failure as an event and then returns it
// fails once: one failure event in the decision output, a close at the next
// sequence, and one result record that carries the private cause.
func TestRuntimeDecisionContinuationFailureIsReportedOnce(t *testing.T) {
	backend := &decisionOutputBackend{MemoryBackend: sessionruntime.NewMemoryBackend()}
	manager, handle := newWaitingDecisionRuntime(t, backend)
	logger, logs := agenttest.CaptureLogs()
	service := continuationService(t, manager, withLogger(logger))
	manager.SetTerminalObserver(func(ctx context.Context, terminal sessionruntime.TerminalRun) {
		outcome.LogRunResult(ctx, logger, terminal)
	})
	failure := apperror.Wrap(apperror.CodeAgentResponseTimeout, errors.New("private idle timeout detail"), nil)

	continueDecision(t, service, manager, decisionCommand(handle, "answer-timeout"), scriptedRun([]native.StreamEvent{
		{Type: native.EventError, Code: string(apperror.CodeAgentResponseTimeout), Cause: failure},
	}, failure))

	var outputs []native.StreamEvent
	for _, entry := range backend.output {
		var event native.StreamEvent
		if entry.Output != nil && json.Unmarshal(entry.Output, &event) == nil {
			outputs = append(outputs, event)
		}
	}
	failures := 0
	for _, event := range outputs {
		if event.Type == native.EventError {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("decision output failure events = %d, want 1", failures)
	}
	if n := len(backend.output); n == 0 || backend.output[n-1].Type != "decision_output_end" {
		t.Fatal("decision output did not close")
	}
	if n := len(backend.output); n != len(outputs)+1 {
		t.Fatalf("decision output = %d entries, want its events and one close", n)
	}
	records := waitRunResults(t, logs, 1)
	if got := agenttest.RecordAttrs(records[0])["error"]; !strings.Contains(got, "private idle timeout detail") {
		t.Fatalf("result record error = %q, want the private cause", got)
	}
	for _, record := range logs.AtLevel(slog.LevelError) {
		if record.Message != "agent run" {
			t.Fatalf("unexpected error record %q besides the run result", record.Message)
		}
	}
	if view := runStatus(t, manager); view == nil || view.Status != sessionruntime.RunStatusErrored || view.ErrorCode != string(apperror.CodeAgentResponseTimeout) {
		t.Fatalf("terminal run = %#v, want errored %s", view, apperror.CodeAgentResponseTimeout)
	}
}

func newWaitingDecisionRuntime(t *testing.T, backends ...sessionruntime.Backend) (*sessionruntime.Manager, sessionruntime.RunHandle) {
	t.Helper()
	var backend sessionruntime.Backend = sessionruntime.NewMemoryBackend()
	if len(backends) > 0 {
		backend = backends[0]
	}
	manager := sessiontest.New(backend, sessionruntime.Options{
		OwnerID:       "runtime-lifecycle-owner",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Second,
		CommandAckTTL: time.Second,
	})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start runtime manager: %v", err)
	}
	handle, err := sessiontest.Start(context.Background(), manager,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		agenttest.LifecycleRunID,
		make(chan struct{}, 1),
		func() {},
		make(chan turn.InjectMessage, 1),
	)
	if err != nil {
		t.Fatalf("start runtime run: %v", err)
	}
	if _, err := manager.HandleAgentEvent(context.Background(), handle, native.StreamEvent{
		Type:        native.EventUserInputRequest,
		UserInputID: "44444444-4444-4444-8444-444444444444",
		Status:      "pending",
	}); err != nil {
		t.Fatalf("park runtime decision: %v", err)
	}
	if _, err := manager.FinishRun(context.Background(), handle, ""); err != nil {
		t.Fatalf("mark deferred producer ready: %v", err)
	}
	return manager, handle
}

type failNextRuntimeDecisionBackend struct {
	*sessionruntime.MemoryBackend
	failNext atomic.Bool
	err      error
}

func (b *failNextRuntimeDecisionBackend) Update(
	ctx context.Context,
	key sessionruntime.Key,
	update sessionruntime.SnapshotUpdate,
) (sessionruntime.Snapshot, bool, error) {
	if b.failNext.CompareAndSwap(true, false) {
		return sessionruntime.Snapshot{}, false, b.err
	}
	return b.MemoryBackend.Update(ctx, key, update)
}

// A model call the provider cancelled is a failure of the run, not a stop: the
// run's context is still live, so nobody asked it to end, and a run that
// failed must not be left parked on the decision it already answered.
func TestContinueRuntimeDecisionDoesNotParkProviderCancellation(t *testing.T) {
	manager, handle := newWaitingDecisionRuntime(t)
	service := continuationService(t, manager)

	continueDecision(t, service, manager, decisionCommand(handle, "provider-cancel"), scriptedRun(nil, context.Canceled))

	if view := runStatus(t, manager); view == nil || view.Status != sessionruntime.RunStatusErrored {
		t.Fatalf("terminal run = %#v, want errored instead of parked", view)
	}
}

// A refused projection write cancels the continuation with the refusal, not a
// bare cancellation, so the run fails under the publication's own code.
func TestContinueRuntimeDecisionCancelsContinuationAfterPublicationFailure(t *testing.T) {
	backend := &failNextRuntimeDecisionBackend{
		MemoryBackend: sessionruntime.NewMemoryBackend(),
		err:           errors.New("private runtime publication failure"),
	}
	manager, handle := newWaitingDecisionRuntime(t, backend)
	service := continuationService(t, manager)
	continuationCause := make(chan error, 1)
	exec := runFuncExecutor(func(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
		backend.failNext.Store(true)
		emit(native.StreamEvent{Type: native.EventTextDelta, Delta: "late"})
		<-ctx.Done()
		continuationCause <- context.Cause(ctx)
		return drive.ExecEnding{Cause: context.Cause(ctx), Phase: outcome.PhaseExecution}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		continueDecision(t, service, manager, decisionCommand(handle, "publish-failure"), func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{Exec: exec}, nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime decision continuation hung after publication failure")
	}

	cause := <-continuationCause
	var refused drive.PublishRefused
	if !errors.As(cause, &refused) {
		t.Fatalf("continuation cause = %v, want the publication refusal", cause)
	}
	view := runStatus(t, manager)
	if view == nil || view.Status != sessionruntime.RunStatusErrored {
		t.Fatalf("terminal run = %#v, want errored", view)
	}
	if view.ErrorCode != string(apperror.CodeSessionPublishFailed) {
		t.Fatalf("terminal run error code = %q, want %s", view.ErrorCode, apperror.CodeSessionPublishFailed)
	}
}

// runFuncExecutor is an executor written inline by the test that needs one.
type runFuncExecutor func(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding

func (f runFuncExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	ending := f(ctx, emit)
	if ending.Commit == nil {
		ending.Commit = func(context.Context, *outcome.RunFailure, bool) (drive.CommitSummary, error) {
			return drive.CommitSummary{}, nil
		}
	}
	return ending
}

// lifecycleRow is one lifecycle row a run left, whichever write produced it:
// an unfenced run writes it at once, a fenced run when its terminal state is
// observed.
type lifecycleRow struct {
	Status   string
	Snapshot []byte
}

func lifecycleRows(store *agenttest.RecordingLifecycleStore) []lifecycleRow {
	var rows []lifecycleRow
	for _, row := range store.Creates {
		rows = append(rows, lifecycleRow{Status: row.Status, Snapshot: row.Snapshot})
	}
	for _, row := range store.TerminalUpserts {
		rows = append(rows, lifecycleRow{Status: row.Status, Snapshot: row.Snapshot})
	}
	return rows
}

// lifecycleRun is a scripted run whose plan carries the context lifecycle a
// resolved native run would, so the run's ending is what writes it.
func lifecycleRun(service *Service, events []native.StreamEvent) func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
	run := scriptedRun(events, nil)
	return func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		plan, err := run(ctx, req)
		plan.Lifecycle = service.lifecycle.Terminal(ctx, agenttest.LifecycleRunConfig())
		return plan, err
	}
}

func TestContinueRuntimeDecisionPersistsCurrentStackTerminalOutcomes(t *testing.T) {
	tests := []struct {
		name              string
		events            []native.StreamEvent
		wantLifecycle     string
		wantRuntimeStatus string
	}{
		{
			name:              "completed",
			events:            []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventAgentEnd}},
			wantLifecycle:     lifecycle.StatusCompleted,
			wantRuntimeStatus: sessionruntime.RunStatusCompleted,
		},
		{
			name: "provider failure",
			events: []native.StreamEvent{
				{Type: native.EventAgentStart},
				{Type: native.EventError, Error: "private provider detail"},
				{Type: native.EventAgentAbort},
			},
			wantLifecycle:     lifecycle.StatusFailedProvider,
			wantRuntimeStatus: sessionruntime.RunStatusErrored,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, handle := newWaitingDecisionRuntime(t)
			lifecycles := &agenttest.RecordingLifecycleStore{}
			service := continuationService(t, manager, withLifecycleStore(lifecycles))
			manager.SetTerminalObserver(service.lifecycle.ReconcileTerminal)

			continueDecision(t, service, manager, decisionCommand(handle, "outcome"), lifecycleRun(service, tt.events))

			rows := lifecycleRows(lifecycles)
			if len(rows) != 1 || rows[0].Status != tt.wantLifecycle {
				t.Fatalf("lifecycle rows = %#v, want one %s row", rows, tt.wantLifecycle)
			}
			if bytes.Contains(rows[0].Snapshot, []byte("private provider detail")) {
				t.Fatalf("lifecycle snapshot leaked private provider detail: %s", rows[0].Snapshot)
			}
			if view := runStatus(t, manager); view == nil || view.Status != tt.wantRuntimeStatus {
				t.Fatalf("runtime terminal = %#v, want %s", view, tt.wantRuntimeStatus)
			}
		})
	}
}

// A continuation that asks a second question parks the same run again: the
// run waits on the new decision, its lifecycle is not ended, and the run is
// still there for the next answer to resume.
func TestContinueRuntimeDecisionDefersSecondPendingDecisionLifecycle(t *testing.T) {
	tests := []struct {
		name     string
		request  native.StreamEvent
		terminal native.StreamEvent
	}{
		{
			name:     "tool approval",
			request:  native.StreamEvent{Type: native.EventToolApprovalRequest, ApprovalID: "approval-next", Status: "pending"},
			terminal: native.StreamEvent{Type: native.EventAgentEnd, ApprovalID: "approval-next", Status: "pending"},
		},
		{
			name:     "user input",
			request:  native.StreamEvent{Type: native.EventUserInputRequest, UserInputID: "input-next", Status: "pending"},
			terminal: native.StreamEvent{Type: native.EventAgentEnd, ApprovalID: "input-next", UserInputID: "input-next", Status: "pending"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, handle := newWaitingDecisionRuntime(t)
			lifecycles := &agenttest.RecordingLifecycleStore{}
			service := continuationService(t, manager, withLifecycleStore(lifecycles))
			manager.SetTerminalObserver(service.lifecycle.ReconcileTerminal)

			continueDecision(t, service, manager, decisionCommand(handle, "second"), lifecycleRun(service, []native.StreamEvent{
				{Type: native.EventAgentStart}, tt.request, tt.terminal,
			}))

			if rows := lifecycleRows(lifecycles); len(rows) != 0 {
				t.Fatalf("lifecycle rows = %#v, want none while a second decision is pending", rows)
			}
			if view := runStatus(t, manager); view == nil || view.Status != sessionruntime.RunStatusWaitingDecision {
				t.Fatalf("runtime state = %#v, want waiting_decision", view)
			}
			// The run still takes the next answer.
			_, cancel, _, err := manager.DecisionContinuationContext(decisionCommand(handle, "third"))
			if err != nil {
				t.Fatalf("re-parked run refused the next answer: %v", err)
			}
			cancel()
		})
	}
}

// A continuation stopped while it runs ends aborted, not errored: the stop is
// what ended it, even though the model call it interrupted reports a failure.
func TestContinueRuntimeDecisionStopEndsAborted(t *testing.T) {
	manager, handle := newWaitingDecisionRuntime(t)
	service := continuationService(t, manager)
	exec := runFuncExecutor(func(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
		emit(native.StreamEvent{Type: native.EventAgentStart})
		<-ctx.Done()
		// The interrupted model call reports the cancellation as its failure,
		// wrapped the way a provider client wraps it.
		emit(native.StreamEvent{Type: native.EventError, Cause: errors.Join(errors.New("stream read"), context.Cause(ctx))})
		return drive.ExecEnding{Terminal: &native.StreamEvent{Type: native.EventAgentAbort}}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		continueDecision(t, service, manager, decisionCommand(handle, "stop"), func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
			return drive.RunPlan{Exec: exec}, nil
		})
	}()
	waitRunStatus(t, manager, sessionruntime.RunStatusRunning)
	if applied, err := manager.AbortControl(context.Background(), agenttest.LifecycleBotID, agenttest.LifecycleSessionID, agenttest.LifecycleRunID, "abort-continuation"); err != nil || !applied {
		t.Fatalf("AbortControl() = (%t, %v), want (true, nil)", applied, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopped continuation did not end")
	}

	view := runStatus(t, manager)
	if view == nil || view.Status != sessionruntime.RunStatusAborted || view.ErrorCode != "" {
		t.Fatalf("runtime terminal = %#v, want aborted with no code", view)
	}
}

func waitRunStatus(t *testing.T, manager *sessionruntime.Manager, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if view := runStatus(t, manager); view != nil && view.Status == status {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run never reached %s", status)
}

// A continuation whose command no longer targets the live run lost it: it
// writes nothing, neither a lifecycle row nor a terminal state, and does not
// run its model call.
func TestContinueRuntimeDecisionOwnershipLossDoesNotPersistLifecycle(t *testing.T) {
	manager, handle := newWaitingDecisionRuntime(t)
	lifecycles := &agenttest.RecordingLifecycleStore{}
	service := continuationService(t, manager, withLifecycleStore(lifecycles))
	stale := decisionCommand(handle, "stale")
	stale.Generation += "-stale"

	continueDecision(t, service, manager, stale, func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		t.Fatal("stale continuation unexpectedly ran")
		return drive.RunPlan{}, nil
	})

	if rows := lifecycleRows(lifecycles); len(rows) != 0 {
		t.Fatalf("ownership-lost continuation created lifecycle rows: %#v", rows)
	}
	if view := runStatus(t, manager); view == nil || view.Status != sessionruntime.RunStatusWaitingDecision {
		t.Fatalf("runtime state = %#v, want the run left to its owner", view)
	}
}

func TestStopTurnAbortsParkedDecision(t *testing.T) {
	manager, _ := newWaitingDecisionRuntime(t)
	service := newTestService(t, Deps{SessionRuntime: manager, AllowedTeam: "team-1"})
	cmd := turn.StopCommand{TeamID: "other-team", BotID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID}
	if _, err := service.StopTurn(context.Background(), cmd); !errors.Is(err, turn.ErrTeamNotServed) {
		t.Fatalf("wrong team: %v", err)
	}
	cmd.TeamID = "team-1"
	stopped, err := service.StopTurn(context.Background(), cmd)
	if err != nil || !stopped {
		t.Fatalf("stop = %t, %v", stopped, err)
	}
	snapshot, err := manager.Snapshot(context.Background(), cmd.BotID, cmd.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentRunView != nil && snapshot.CurrentRunView.Status != sessionruntime.RunStatusAborted {
		t.Fatalf("run = %#v", snapshot.CurrentRunView)
	}
}

// Both decision kinds resume as the turn the decision parked: the request
// carries that turn's request message, so a failure row the continuation
// leaves, even one from its setup, is filed under the turn rather than as a
// turn of its own.
func TestDecisionContinuationsResumeAsTheParkedTurn(t *testing.T) {
	t.Parallel()

	service := newTestService(t, Deps{Messages: &continuationTurnRequestResolver{requestMessageID: "request-message-id"}})
	handle := sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, TurnID: "turn-id"}
	ctx := context.Background()
	for name, cont := range map[string]answer.Continuation{
		"tool approval": service.answer.ToolApprovalContinuation(ctx, decide.NewCommittedToolApproval(toolapproval.Request{ID: "approval", SessionID: agenttest.LifecycleSessionID}, decide.ToolApprovalResponseInput{BotID: agenttest.LifecycleBotID}, false).ForRun(handle.RunID, handle), true),
		"user input":    service.answer.UserInputContinuation(ctx, decide.NewCommittedUserInput(userinput.Request{ID: "input", SessionID: agenttest.LifecycleSessionID}, decide.UserInputResponseInput{BotID: agenttest.LifecycleBotID}).ForRun(handle.RunID, handle), true),
	} {
		req := cont.Req
		if !req.UserMessagePersisted || req.PersistedUserMessageID != "request-message-id" {
			t.Fatalf("%s: request = persisted:%t message:%q, want the parked turn's request message", name, req.UserMessagePersisted, req.PersistedUserMessageID)
		}
		if req.RunHandle != handle || req.RunID != handle.RunID || req.ThreadID != agenttest.LifecycleSessionID {
			t.Fatalf("%s: request run = %#v (%q), want the parked run", name, req.RunHandle, req.RunID)
		}
	}
}

// A routed continuation whose setup fails, such as storing the answer's
// result, leaves its failure row as the unrouted one does: the answer was
// accepted when the command committed it.
func TestRoutedContinuationSetupFailureLeavesAFailureRow(t *testing.T) {
	manager, handle := newWaitingDecisionRuntime(t)
	messages := &agenttest.RecordingMessages{}
	service := continuationService(t, manager, withDeps(func(d *Deps) { d.Messages = messages }))

	continueDecision(t, service, manager, decisionCommand(handle, "setup-failure"), func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{}, errors.New("SECRET tool result store unavailable")
	})

	if len(messages.Persisted) != 1 || messages.Persisted[0].Role != "assistant" {
		t.Fatalf("persisted %#v, want one assistant failure row and no user row", messages.Persisted)
	}
	if view := runStatus(t, manager); view == nil || view.Status != sessionruntime.RunStatusErrored || view.ErrorCode != string(apperror.CodeRuntimeRunFailed) {
		t.Fatalf("terminal run = %#v, want errored %s", view, apperror.CodeRuntimeRunFailed)
	}
}

// A run that parks has not ended, so the lifecycle row of the context it
// assembled is not written yet: the continuation that resumes it writes the
// row once the run does end.
func TestParkedRunDoesNotEndItsLifecycle(t *testing.T) {
	lifecycles := &agenttest.RecordingLifecycleStore{}
	svc := newDriveService(t, nil, &agenttest.ScriptedAdmitter{Started: true}, withLifecycleStore(lifecycles))
	terminal := native.StreamEvent{Type: native.EventAgentEnd, ApprovalID: "next", Status: "pending"}
	exec := &drivetest.ScriptedExecutor{Terminal: &terminal, Deferred: true}
	spec := drivetest.ScheduledSpec(exec, drive.NoDelivery{})
	spec.Setup = func(ctx context.Context, _ runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: exec, Lifecycle: svc.lifecycle.Terminal(ctx, agenttest.LifecycleRunConfig())}, nil
	}

	result := svc.driver.Drive(context.Background(), spec)

	if result.End != outcome.EndParked {
		t.Fatalf("end = %s, want parked", result.End)
	}
	if rows := lifecycleRows(lifecycles); len(rows) != 0 {
		t.Fatalf("lifecycle rows = %#v, want none for a parked run", rows)
	}
}
