package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agentapi"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/answer"
	"github.com/felinics/memoh/internal/agent/internal/decide"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/drive/drivetest"
	"github.com/felinics/memoh/internal/agent/internal/lifecycle"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/bots"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/runtimekind"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
	sessiontest "github.com/felinics/memoh/internal/testutil/sessionruntime"
)

// Scenario 7 (X7): a decision continuation that fails after the run resumed.
//
// Current behavior: session_runs records the cause's catalogued code in
// error_code and no message; a cause without one finishes as
// runtime_run_failed. A continuation stopped while it ran is aborted with no
// code, even when the call it interrupted wraps the cancellation in a failure
// of its own.
func TestDecisionContinuationFailureRecordsRunColumns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cause   error
		stopped bool
		want    [3]string
	}{
		{"coded cause", apperror.Wrap(apperror.CodeSessionHistoryInconsistent, errors.New("SECRET"), nil), false, [3]string{"failed", "session_runtime.history_inconsistent", ""}},
		{"plain cause", errors.New("SECRET continuation failed"), false, [3]string{"failed", "runtime_run_failed", ""}},
		{"wrapped cancellation", fmt.Errorf("SECRET provider stream: %w", context.Canceled), true, [3]string{"aborted", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := sessionledger.New()
			manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
				OwnerID: "owner-continuation", OwnerLeaseTTL: time.Minute, Ledger: runs, Fence: agenttest.NoopFence{},
			})
			t.Cleanup(func() { _ = manager.Close() })
			admission, err := manager.Admit(context.Background(), sessionruntime.AdmitInput{
				BotID: "bot-1", SessionID: "session-1", InvocationID: "invocation-continuation", Payload: []byte(`{}`),
				Execution: sessionruntime.Execution{
					Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
						return sessionruntime.RunAdmissionView{}, nil
					},
				},
			})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			service := continuationService(t, manager)
			command := sessionruntime.Command{
				BotID: admission.Handle.BotID, SessionID: admission.Handle.SessionID,
				RunID: admission.Handle.RunID, Generation: admission.Handle.Generation,
			}
			runCtx, release, handle, err := manager.DecisionContinuationContext(command)
			if err != nil {
				t.Fatalf("decision continuation context: %v", err)
			}
			defer release()
			if tc.stopped {
				var stop context.CancelFunc
				runCtx, stop = context.WithCancel(runCtx)
				stop()
			}
			service.answer.DriveRouted(runCtx, command, handle, answer.Continuation{
				Req: runreq.ChatRequest{BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle, UserMessagePersisted: true},
				Setup: func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
					return drive.RunPlan{}, tc.cause
				},
			})
			run, err := runs.Get(context.Background(), admission.RunID)
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if got := [3]string{string(run.State), run.ErrorCode, run.ErrorMessage}; got != tc.want {
				t.Fatalf("session_runs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInlineRuntimeDecisionClosesOutputWithoutFinishingProducer(t *testing.T) {
	for _, runtimeType := range []string{session.RuntimeClaudeCode, session.RuntimeCodex, string(runtimekind.GrokBuild), session.RuntimeACPAgent} {
		for _, canceled := range []bool{false, true} {
			t.Run(runtimeType+map[bool]string{false: "/submit", true: "/cancel"}[canceled], func(t *testing.T) {
				ctx := context.Background()
				backend := sessionruntime.NewMemoryBackend()
				manager := sessiontest.New(backend, sessionruntime.Options{OwnerID: "inline-test", StateTTL: time.Minute})
				t.Cleanup(func() { _ = manager.Close() })
				if err := manager.Start(ctx); err != nil {
					t.Fatal(err)
				}
				handle, err := sessiontest.Start(ctx, manager, agenttest.LifecycleBotID, agenttest.LifecycleSessionID, agenttest.LifecycleRunID,
					make(chan struct{}, 1), func() {}, make(chan turn.InjectMessage, 1))
				if err != nil {
					t.Fatal(err)
				}
				manager.MarkInlineDecisionRun(handle.BotID, handle.SessionID, handle.RunID)
				request := userinput.Request{
					ID: "44444444-4444-4444-8444-444444444444", BotID: handle.BotID,
					SessionID: handle.SessionID, ToolCallID: "question", ToolName: "AskUserQuestion", Status: userinput.StatusPending,
				}
				if _, err := manager.HandleAgentEvent(ctx, handle, native.StreamEvent{
					Type:        native.EventUserInputRequest,
					UserInputID: request.ID, ToolCallID: request.ToolCallID, Status: request.Status,
				}); err != nil {
					t.Fatal(err)
				}
				resolved := request
				resolved.Status = userinput.StatusSubmitted
				if canceled {
					resolved.Status = userinput.StatusCanceled
				}
				inputs := &agenttest.UserInput{Target: request, Resolved: resolved}
				var logs bytes.Buffer
				service := newTestService(t, Deps{
					SessionRuntime: manager, Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
					BotPermissions: &agenttest.PermissionChecker{Values: map[string]bool{handle.BotID + ":" + testACPUserInputOwnerID + ":" + bots.PermissionWorkspaceExec: true}},
					Sessions: &agenttest.Sessions{GetFn: func(context.Context, string) (session.Thread, error) {
						return session.Thread{
							ID: handle.SessionID, BotID: handle.BotID, RuntimeType: runtimeType,
							RuntimeMetadata: map[string]any{"runtime_owner_account_id": testACPUserInputOwnerID},
						}, nil
					}},
				}, withDecide(func(d *decide.Deps) { d.UserInput = inputs }))
				payload, err := json.Marshal(agentapi.UserInputResponseInput{
					ActorUserID: testACPUserInputOwnerID, Canceled: canceled,
					Answers: []userinput.QuestionAnswer{{QuestionID: "q", Text: "answer"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				command := sessionruntime.Command{
					ID: "inline-answer", Type: sessionruntime.CommandUserInputResponse,
					BotID: handle.BotID, SessionID: handle.SessionID, RunID: handle.RunID, Generation: handle.Generation,
					TargetID: request.ID, Payload: payload, StreamOutput: true,
				}
				if err := service.answer.HandleCommand(ctx, command); err != nil {
					t.Fatal(err)
				}
				page, err := backend.ReadDecisionOutput(ctx, sessionruntime.DecisionOutputRef{BotID: handle.BotID, CommandID: command.ID}, 0)
				if err != nil || !page.Done || page.Length != 0 {
					t.Fatalf("inline ACK must close immediately without agent events: %+v, %v", page, err)
				}
				if inputs.SubmitCalls+inputs.CancelCalls != 1 {
					t.Fatal("answer was not committed exactly once")
				}
				// A later native output is still accepted: the ACK must not end the run.
				if _, err := manager.HandleAgentEvent(ctx, handle, native.StreamEvent{Type: native.EventTextDelta, Delta: "after answer"}); err != nil {
					t.Fatal(err)
				}
				snapshot, err := manager.Snapshot(ctx, handle.BotID, handle.SessionID)
				if err != nil || snapshot.CurrentRunView == nil || snapshot.CurrentRunView.Status != sessionruntime.RunStatusRunning || len(snapshot.CurrentRunView.Messages) == 0 {
					t.Fatalf("original producer stopped after answer: %+v, %v", snapshot.CurrentRunView, err)
				}
				if _, err := manager.FinishRun(ctx, handle, sessionruntime.RunStatusCompleted); err != nil {
					t.Fatal(err)
				}
				if logs.Len() != 0 {
					t.Fatalf("successful inline response logged a failure: %s", logs.String())
				}
			})
		}
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

	service := newTestService(t, Deps{Messages: &continuationTurnRequestResolver{RecordingMessages: &agenttest.RecordingMessages{}, requestMessageID: "request-message-id"}})
	handle := sessionruntime.RunHandle{BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, RunID: agenttest.LifecycleRunID, TurnID: "turn-id"}
	ctx := context.Background()
	for name, cont := range map[string]answer.Continuation{
		"tool approval": service.answer.ToolApprovalContinuation(ctx, decide.NewCommittedToolApproval(toolapproval.Request{ID: "approval", SessionID: agenttest.LifecycleSessionID}, agentapi.ToolApprovalResponseInput{BotID: agenttest.LifecycleBotID}, false).ForRun(handle.RunID, handle), true),
		"user input":    service.answer.UserInputContinuation(ctx, decide.NewCommittedUserInput(userinput.Request{ID: "input", SessionID: agenttest.LifecycleSessionID}, agentapi.UserInputResponseInput{BotID: agenttest.LifecycleBotID}).ForRun(handle.RunID, handle), true),
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

type continuationTurnRequestResolver struct {
	*agenttest.RecordingMessages
	requestMessageID string
	sessionID        string
	runID            string
	rootTurnID       string
}

func (s *continuationTurnRequestResolver) GetRunTurnRequest(_ context.Context, sessionID, runID, rootTurnID string) (messagepkg.RunTurnRequest, error) {
	s.sessionID, s.runID, s.rootTurnID = sessionID, runID, rootTurnID
	return messagepkg.RunTurnRequest{MessageID: s.requestMessageID, TurnID: "request-turn-id", TurnPosition: 7}, nil
}

// A live management approval is consumed by the call that prepared it, so
// answering it never replays the tool.
func TestCapabilityApprovalDoesNotReplay(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mcp_manage", "app_manage"} {
		t.Run(name, func(t *testing.T) {
			service := newTestService(t, Deps{})
			target := toolapproval.Request{ID: "approval-1", BotID: "bot-1", ToolName: name, Operation: toolapproval.OperationPermission, Status: toolapproval.StatusApproved}
			// No agent is configured: a replay would fail instead of acknowledging
			// the decision consumed by the already-running prepared operation.
			committed := decide.NewCommittedToolApproval(target, agentapi.ToolApprovalResponseInput{}, true)
			if err := service.continueCommittedToolApprovalResponse(context.Background(), committed, nil); err != nil {
				t.Fatalf("live management approval must not replay: %v", err)
			}
		})
	}
}
