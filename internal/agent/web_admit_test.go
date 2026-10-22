package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/drive/drivetest"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/internal/web"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

func ledgerRun(t *testing.T, runs *sessionledger.Store, runID string) ledger.Run {
	t.Helper()
	run, err := runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("load ledger run %s: %v", runID, err)
	}
	return run
}

// The client is told the run's name, its turn and the cursor it became
// observable at before anything executes: admission alone decides them, and
// nothing the run does could name them afterwards.
func TestAdmitWebRunNamesTheRunBeforeItExecutes(t *testing.T) {
	t.Parallel()

	svc, _, _ := newWebTestRuntime(t)
	var built sessionruntime.RunHandle
	admission, err := admitWebRun(context.Background(), svc, agentapi.WebAdmitInput{
		BotID:        webTestBotID,
		SessionID:    webTestSessionID,
		InvocationID: "invocation-1",
		Payload:      []byte(`{"kind":"message","text":"hi"}`),
		Admission: func(_ context.Context, handle sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			built = handle
			return sessionruntime.RunAdmissionView{}, nil
		},
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	if admission.Duplicate() || admission.RunID() == "" || admission.TurnID() == "" || admission.Cursor().Epoch == "" {
		t.Fatalf("admission = %+v, want a started run with its turn and cursor", admission)
	}
	if built.RunID != admission.RunID() || built.FencingToken <= 0 {
		t.Fatalf("admission view built for %+v, want the admitted run with its fence", built)
	}

	// Nothing has run yet: the run executes only once the entry starts it, so
	// the acceptance the client is sent cannot trail the run's own output.
	executed := make(chan struct{})
	exec := &drivetest.ScriptedExecutor{Terminal: ptrTo(agenttest.TerminalEvent(t, "done"))}
	delivery := newWebTestDelivery()
	spec := webTestSpec(admission, delivery, exec)
	setup := spec.Setup
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		close(executed)
		return setup(ctx, req)
	}
	select {
	case <-executed:
		t.Fatal("the run executed before its entry started it")
	default:
	}
	svc.web.Start(admission, spec)
	if _, ended := delivery.wait(t); ended.State != string(ledger.StateCompleted) {
		t.Fatalf("ended = %+v, want completed", ended)
	}
}

// The account a Web send was made under is recorded on the run, which is what
// decides who may answer the run's approvals.
func TestAdmitWebRunRecordsTheSenderAsInitiator(t *testing.T) {
	t.Parallel()

	svc, _, runs := newWebTestRuntime(t)
	admission, err := admitWebRun(context.Background(), svc, agentapi.WebAdmitInput{
		BotID:        webTestBotID,
		SessionID:    webTestSessionID,
		InvocationID: "invocation-1",
		InitiatorID:  "account-1",
		Payload:      []byte(`{"kind":"message","text":"hi"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	if got := ledgerRun(t, runs, admission.RunID()).InitiatorID; got != "account-1" {
		t.Fatalf("run initiator = %q, want account-1", got)
	}
	delivery := newWebTestDelivery()
	svc.web.Start(admission, webTestSpec(admission, delivery, &drivetest.ScriptedExecutor{Terminal: ptrTo(agenttest.TerminalEvent(t, "done"))}))
	delivery.wait(t)
}

// A redelivered invocation names the run it already has and executes nothing.
func TestAdmitWebRunRedeliveryIsADuplicate(t *testing.T) {
	t.Parallel()

	svc, _, _ := newWebTestRuntime(t)
	first := admitWebTestRun(t, svc, "invocation-1")
	delivery := newWebTestDelivery()
	svc.web.Start(first, webTestSpec(first, delivery, &drivetest.ScriptedExecutor{Terminal: ptrTo(agenttest.TerminalEvent(t, "done"))}))
	delivery.wait(t)

	again := admitWebTestRun(t, svc, "invocation-1")
	if !again.Duplicate() || again.RunID() != first.RunID() || again.TurnID() != first.TurnID() {
		t.Fatalf("redelivery = %+v, want a duplicate of run %s", again, first.RunID())
	}
	if again.Cursor() != (sessionruntime.Cursor{}) {
		t.Fatalf("duplicate cursor = %+v, want none: a duplicate reserved nothing", again.Cursor())
	}
}

// A session already running a turn refuses another, with the sentinel the
// client branches on.
func TestAdmitWebRunRefusesABusySession(t *testing.T) {
	t.Parallel()

	svc, _, _ := newWebTestRuntime(t)
	running := admitWebTestRun(t, svc, "invocation-1")
	exec := &blockingExecutor{started: make(chan struct{})}
	delivery := newWebTestDelivery()
	svc.web.Start(running, webTestSpec(running, delivery, exec))
	awaitStarted(t, exec.started)
	t.Cleanup(func() {
		running.Stop(outcome.ErrRunStopped)
		delivery.wait(t)
	})

	_, err := admitWebRun(context.Background(), svc, agentapi.WebAdmitInput{
		BotID: webTestBotID, SessionID: webTestSessionID, InvocationID: "invocation-2",
		Payload: []byte(`{"kind":"message","text":"second"}`),
	})
	if !errors.Is(err, sessionruntime.ErrSessionBusy) {
		t.Fatalf("AdmitWebRun(busy) error = %v, want ErrSessionBusy", err)
	}
}

// Without a durable queue there is nowhere for a busy send to wait, so it is
// refused as busy and the queue's turn is never built.
func TestAdmitWebRunWithoutAQueueRefusesABusySendWithoutBuildingItsQueueTurn(t *testing.T) {
	t.Parallel()

	svc, _, _ := newWebTestRuntime(t)
	running := admitWebTestRun(t, svc, "invocation-1")
	exec := &blockingExecutor{started: make(chan struct{})}
	delivery := newWebTestDelivery()
	svc.web.Start(running, webTestSpec(running, delivery, exec))
	awaitStarted(t, exec.started)
	t.Cleanup(func() {
		running.Stop(outcome.ErrRunStopped)
		delivery.wait(t)
	})

	built := false
	_, err := admitWebRun(context.Background(), svc, agentapi.WebAdmitInput{
		BotID: webTestBotID, SessionID: webTestSessionID, InvocationID: "invocation-2",
		Payload: []byte(`{"kind":"message","text":"second"}`),
		QueueTurn: func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error) {
			built = true
			return turn.StartTurnCommand{}, nil, nil
		},
	})
	if !errors.Is(err, sessionruntime.ErrSessionBusy) {
		t.Fatalf("AdmitWebRun(busy, no queue) error = %v, want ErrSessionBusy", err)
	}
	if built {
		t.Fatal("the queue's turn was built for a send that cannot wait")
	}
}

// The run's end releases the session's one active slot: the next submission
// is admitted rather than told the session is busy until a lease times out.
func TestWebRunEndReleasesTheSession(t *testing.T) {
	t.Parallel()

	svc, _, runs := newWebTestRuntime(t)
	first := admitWebTestRun(t, svc, "invocation-1")
	delivery := newWebTestDelivery()
	svc.web.Start(first, webTestSpec(first, delivery, &drivetest.ScriptedExecutor{Terminal: ptrTo(agenttest.TerminalEvent(t, "done"))}))
	delivery.wait(t)
	if got := ledgerRun(t, runs, first.RunID()).State; got != ledger.StateCompleted {
		t.Fatalf("first run state = %q, want completed", got)
	}

	next := admitWebTestRun(t, svc, "invocation-2")
	if next.Duplicate() || next.RunID() == first.RunID() {
		t.Fatalf("next admission = %+v, want a new run", next)
	}
	next.Stop(outcome.ErrRunStopped)
	next.Release()
}

// #1105: a Web run whose ending's history write fails must end failed, and its
// subscribers must never be shown it finishing. Publishing the terminal before
// the write is what made a failed write read as a completed turn.
func TestWebHistoryWriteFailureEndsTheRunFailedWithoutFinishing(t *testing.T) {
	t.Parallel()

	svc, manager, runs := newWebTestRuntime(t)
	admission := admitWebTestRun(t, svc, "invocation-1")

	subscription, err := manager.Subscribe(context.Background(), webTestBotID, webTestSessionID)
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer subscription.Close()
	statuses := make(chan []string, 1)
	go func() {
		var seen []string
		for ev := range subscription.C {
			seen = append(seen, runStatusesOf(ev)...)
		}
		statuses <- seen
	}()

	exec := &drivetest.ScriptedExecutor{
		Events:    []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "an answer"}},
		Terminal:  ptrTo(agenttest.TerminalEvent(t, "an answer")),
		CommitErr: errors.New("history unavailable"),
	}
	delivery := newWebTestDelivery()
	svc.web.Start(admission, webTestSpec(admission, delivery, exec))
	events, ended := delivery.wait(t)

	run := ledgerRun(t, runs, admission.RunID())
	if run.State != ledger.StateFailed || run.ErrorCode != string(apperror.CodeSessionHistoryInconsistent) {
		t.Fatalf("session_runs = %q %q, want failed %s", run.State, run.ErrorCode, apperror.CodeSessionHistoryInconsistent)
	}
	if ended.ErrorCode != string(apperror.CodeSessionHistoryInconsistent) {
		t.Fatalf("ended = %+v, want the history failure", ended)
	}
	var reported bool
	for _, ev := range events {
		if ev.Event.IsTerminal() {
			t.Fatalf("delivered terminal %#v after a failed history write", ev.Event)
		}
		if ev.Event.Type == native.EventError && ev.Event.Code == string(apperror.CodeSessionHistoryInconsistent) {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("delivered %#v, want the history failure reported to the socket", events)
	}

	subscription.Close()
	for _, status := range <-statuses {
		if status == sessionruntime.RunStatusFinishing {
			t.Fatal("the run was shown finishing although its history write failed")
		}
	}
}

// runStatusesOf is every run status one subscription event shows.
func runStatusesOf(ev sessionruntime.Event) []string {
	var out []string
	if ev.Snapshot != nil && ev.Snapshot.CurrentRunView != nil {
		out = append(out, ev.Snapshot.CurrentRunView.Status)
	}
	if ev.Delta != nil {
		if ev.Delta.CurrentRunView != nil {
			out = append(out, ev.Delta.CurrentRunView.Status)
		}
		if ev.Delta.Run != nil && ev.Delta.Run.Status != nil {
			out = append(out, *ev.Delta.Run.Status)
		}
	}
	return out
}

// A Web run whose publication is refused ends: its subscribers cannot follow a
// run the session runtime will not record, so continuing only produces output
// nobody else sees.
func TestWebPublicationRefusalEndsTheRun(t *testing.T) {
	t.Parallel()

	var manager *sessionruntime.Manager
	svc, manager, runs := newWebTestRuntime(t, withDrive(func(d *drive.Deps) {
		d.Events = agenttest.EventFunc(func(ctx context.Context, handle sessionruntime.RunHandle, ev native.StreamEvent) error {
			if ev.Type == native.EventTextDelta {
				return errors.New("projection unavailable")
			}
			_, err := manager.HandleAgentEvent(ctx, handle, ev)
			return err
		})
	}))
	admission := admitWebTestRun(t, svc, "invocation-1")

	exec := &blockingExecutor{
		started: make(chan struct{}),
		events:  []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "partial"}},
	}
	delivery := newWebTestDelivery()
	svc.web.Start(admission, webTestSpec(admission, delivery, exec))
	events, _ := delivery.wait(t)

	run := ledgerRun(t, runs, admission.RunID())
	if run.State != ledger.StateFailed || run.ErrorCode != string(apperror.CodeSessionPublishFailed) {
		t.Fatalf("session_runs = %q %q, want failed %s", run.State, run.ErrorCode, apperror.CodeSessionPublishFailed)
	}
	var reported bool
	for _, ev := range events {
		if ev.Event.Type == native.EventError && ev.Event.Code == string(apperror.CodeSessionPublishFailed) {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("delivered %#v, want the publication failure reported to the socket", events)
	}
}

// A Web abort is the client asking the run to stop. It is recorded as a stop,
// not as a failure: nothing failed, and an error rate that counts stops tracks
// how often users press stop.
func TestWebStopIsRecordedAsAStop(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		stop func(t *testing.T, manager *sessionruntime.Manager, admission *web.Admission, cancelCaller context.CancelFunc)
	}{
		{
			name: "routed abort",
			stop: func(t *testing.T, manager *sessionruntime.Manager, admission *web.Admission, _ context.CancelFunc) {
				t.Helper()
				if _, err := manager.AbortControl(context.Background(), webTestBotID, webTestSessionID, admission.RunID(), "control-1"); err != nil {
					t.Fatalf("AbortControl() error = %v", err)
				}
			},
		},
		{
			name: "caller context ending",
			stop: func(_ *testing.T, _ *sessionruntime.Manager, _ *web.Admission, cancelCaller context.CancelFunc) {
				cancelCaller()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, manager, runs := newWebTestRuntime(t)
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			admission, err := admitWebRun(callerCtx, svc, agentapi.WebAdmitInput{
				BotID: webTestBotID, SessionID: webTestSessionID, InvocationID: "invocation-1",
				Payload: []byte(`{"kind":"message","text":"hi"}`),
			})
			if err != nil {
				t.Fatalf("AdmitWebRun() error = %v", err)
			}
			exec := &blockingExecutor{started: make(chan struct{}), events: []native.StreamEvent{{Type: native.EventAgentStart}}}
			delivery := newWebTestDelivery()
			svc.web.Start(admission, webTestSpec(admission, delivery, exec))
			awaitStarted(t, exec.started)

			tc.stop(t, manager, admission, cancelCaller)
			events, ended := delivery.wait(t)

			run := ledgerRun(t, runs, admission.RunID())
			if run.State != ledger.StateAborted || run.ErrorCode != "" {
				t.Fatalf("session_runs = %q %q, want aborted without a code", run.State, run.ErrorCode)
			}
			if ended.Failure != nil {
				t.Fatalf("ended failure = %v, want none for a stop", ended.Failure)
			}
			for _, ev := range events {
				if ev.Event.Type == native.EventError {
					t.Fatalf("delivered error %#v for a stop", ev.Event)
				}
			}
		})
	}
}

// The files a Web run's events point at are linked to the reply by the run's
// own history write. Nothing on the socket's side collects them any more, so
// the request the run executes must carry the driver's collector, and it must
// hold what preparation ingested by the time the history is written.
func TestWebSendAssetsReachTheHistoryWrite(t *testing.T) {
	t.Parallel()

	svc, _, _ := newWebTestRuntime(t, withDrive(func(d *drive.Deps) { d.OutputMedia = &agenttest.OutputMedia{} }))
	admission := admitWebTestRun(t, svc, "invocation-1")

	var linked []turn.OutboundAssetRef
	delivery := newWebTestDelivery()
	spec := web.RunSpec(admission, runreq.ChatRequest{BotID: webTestBotID, ThreadID: webTestSessionID}, delivery, outcome.InputHeld)
	spec.Setup = func(_ context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		exec := &collectingExecutor{
			events: []native.StreamEvent{{
				Type:        native.EventAttachment,
				Attachments: []event.FileAttachment{{Type: "image", Path: "/data/out.png"}},
			}},
			commit: func() {
				if req.OutboundAssetCollector != nil {
					linked = req.OutboundAssetCollector()
				}
			},
		}
		return drive.RunPlan{Exec: exec, Req: req}, nil
	}
	svc.web.Start(admission, spec)
	delivery.wait(t)

	if len(linked) != 1 || linked[0].ContentHash != "hash:/data/out.png" {
		t.Fatalf("history write saw assets %#v, want the ingested attachment", linked)
	}
}

// collectingExecutor emits its events and, when its ending is written, runs
// commit: the point at which the history write links the run's assets.
type collectingExecutor struct {
	events []native.StreamEvent
	commit func()
}

func (e *collectingExecutor) Execute(_ context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	for _, ev := range e.events {
		emit(ev)
	}
	terminal := native.StreamEvent{Type: native.EventAgentEnd}
	return drive.ExecEnding{
		Terminal: &terminal,
		Phase:    outcome.PhaseExecution,
		Commit: func(context.Context, *outcome.RunFailure, bool) (drive.CommitSummary, error) {
			e.commit()
			return drive.CommitSummary{}, nil
		},
	}
}

// A runtime that owns its conversation context has no rewind, so a retry of
// one of its turns is refused before the runtime is asked anything.
func TestWebRetryRejectsTurnReplacementForACP(t *testing.T) {
	t.Parallel()

	pool := &recordingACPPrompter{}
	resolver := newTestService(t, Deps{
		Sessions: acpRuntimeSessionServiceForTest("user-1"),
		Messages: driveReplaceableHistory(t),
	}, withTurnAdmitter(agenttest.NewScriptedAdmitter()), withACPPool(pool))
	admission, err := admitWebRun(context.Background(), resolver, agentapi.WebAdmitInput{
		BotID: "bot-1", SessionID: "session-1", InvocationID: "retry-1", Payload: []byte(`{"kind":"retry_message"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	delivery := newWebTestDelivery()
	resolver.RunWebRetry(admission, agentapi.WebRetryInput{
		BotID: "bot-1", SessionID: "session-1", TargetTurnID: driveReplacedTurnID,
	}, delivery)

	if _, ended := delivery.wait(t); ended.ErrorCode != string(apperror.CodeExternalAgentTurnReplacementUnsupported) {
		t.Fatalf("ended = %+v, want %q", ended, apperror.CodeExternalAgentTurnReplacementUnsupported)
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want none for a refused replacement", pool.calls)
	}
}

func requestedSkillGuardRequest(sessionID string) runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID:    "bot-1",
		ThreadID: sessionID,
		RequestedSkills: []runreq.RequestedSkillContext{{
			Name:    "alpha",
			Content: "skill content",
		}},
	}
}

func TestWebSendRejectsACPRequestedSkillsBeforePool(t *testing.T) {
	pool := &recordingACPPrompter{}
	resolver := newTestService(t, Deps{Sessions: acpRuntimeSessionServiceForTest("user-1")}, withWebAdmitter(), withACPPool(pool))

	// The refusal is the run's setup failure, recorded under the catalog code
	// the composer already renders for it.
	if _, err := streamWebSend(context.Background(), t, resolver, requestedSkillGuardRequest("session-1")); apperror.CodeOf(err) != apperror.CodeSlashSkillActivationUnsupported {
		t.Fatalf("Web send failure = %v, want %s", err, apperror.CodeSlashSkillActivationUnsupported)
	}
	if pool.calls != 0 {
		t.Fatalf("ACP pool calls = %d, want 0", pool.calls)
	}
}
