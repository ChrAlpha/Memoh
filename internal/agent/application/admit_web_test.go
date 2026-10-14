package application

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

const (
	webTestBotID     = "11111111-1111-4111-8111-111111111111"
	webTestSessionID = "22222222-2222-4222-8222-222222222222"
)

// webTestDelivery is the socket's end of a Web run in these tests: it keeps
// every event the run delivered and says when the run has ended.
type webTestDelivery struct {
	mu     sync.Mutex
	events []RunEvent
	ended  RunEnded
	done   chan struct{}
}

func newWebTestDelivery() *webTestDelivery {
	return &webTestDelivery{done: make(chan struct{})}
}

func (d *webTestDelivery) Deliver(_ context.Context, ev RunEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, ev)
	return true
}

func (*webTestDelivery) Settle() {}

func (d *webTestDelivery) Ended(_ context.Context, end RunEnded) {
	d.mu.Lock()
	d.ended = end
	d.mu.Unlock()
	close(d.done)
}

// wait returns once the run has ended, with what it delivered.
func (d *webTestDelivery) wait(t *testing.T) ([]RunEvent, RunEnded) {
	t.Helper()
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the Web run did not end")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]RunEvent(nil), d.events...), d.ended
}

// payloads is what the run delivered as the stream of WSStreamEvent the
// pre-driver tests read.
func (d *webTestDelivery) payloads(t *testing.T) chan WSStreamEvent {
	t.Helper()
	events, _ := d.wait(t)
	ch := make(chan WSStreamEvent, len(events))
	for _, ev := range events {
		ch <- ev.Payload
	}
	close(ch)
	return ch
}

// streamWebSend runs req as an admitted Web send, the way the WebSocket does:
// admission names the run, and the send executes under it. A service without
// a session runtime is given one that admits every invocation. It returns the
// events the socket's end received and the run's failure.
func streamWebSend(ctx context.Context, t *testing.T, svc *Service, req ChatRequest) (chan WSStreamEvent, error) {
	t.Helper()
	if svc.sessionRuntime == nil {
		svc.sessionRuntime = &lifecycleTurnAdmitter{admission: webTestAdmission(req)}
	}
	admission, err := svc.AdmitWebRun(ctx, WebAdmitInput{
		BotID:        req.BotID,
		SessionID:    req.ThreadID,
		InvocationID: "web-invocation",
		Payload:      []byte(`{"kind":"message"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	delivery := newWebTestDelivery()
	svc.RunWebSend(admission, req, delivery) //nolint:contextcheck // The run's context is the one admission derived from ctx.
	events := delivery.payloads(t)
	_, ended := delivery.wait(t)
	return events, ended.Failure
}

// webTestAdmission is the run a test's admitter starts for req: the run and
// turn the request names, or fixed ones when it names none.
func webTestAdmission(req ChatRequest) sessionruntime.Admission {
	admission := sessionruntime.Admission{RunID: req.RunID, TurnID: req.TurnID, Started: true}
	if admission.RunID == "" {
		admission.RunID = "run-1"
	}
	if admission.TurnID == "" {
		admission.TurnID = "turn-1"
	}
	if req.TurnPosition != nil {
		admission.TurnPosition = *req.TurnPosition
	}
	admission.Handle = sessionruntime.RunHandle{
		BotID: req.BotID, SessionID: req.ThreadID, RunID: admission.RunID, TurnID: admission.TurnID, FencingToken: 1,
	}
	return admission
}

// newWebTestRuntime is a real session runtime over an in-memory ledger, so a
// Web run is admitted, published and finished the way production does it.
func newWebTestRuntime(t *testing.T, svc *Service) (*sessionruntime.Manager, *sessionledger.Store) {
	t.Helper()
	runs := sessionledger.New()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-web-test",
		OwnerLeaseTTL: time.Minute,
		Ledger:        runs,
		Fence:         abortAlignmentFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	svc.sessionRuntime = manager
	svc.publishTurnEvent = func(ctx context.Context, handle sessionruntime.RunHandle, ev native.StreamEvent) error {
		_, err := manager.HandleAgentEvent(ctx, handle, ev)
		return err
	}
	return manager, runs
}

func newWebTestService() *Service {
	return &Service{logger: slog.New(slog.DiscardHandler)}
}

func admitWebTestRun(t *testing.T, svc *Service, invocationID string) *WebAdmission {
	t.Helper()
	admission, err := svc.AdmitWebRun(context.Background(), WebAdmitInput{
		BotID:        webTestBotID,
		SessionID:    webTestSessionID,
		InvocationID: invocationID,
		Payload:      []byte(`{"kind":"message","text":"` + invocationID + `"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun(%s) error = %v", invocationID, err)
	}
	return admission
}

// webTestSpec is a Web send whose setup hands the driver exec.
func webTestSpec(svc *Service, admission *WebAdmission, delivery RunDelivery, exec runExecutor) runSpec {
	spec := svc.webRunSpec(admission, ChatRequest{BotID: webTestBotID, ThreadID: webTestSessionID}, delivery, inputHeld)
	spec.Setup = func(_ context.Context, req ChatRequest) (runPlan, error) {
		return runPlan{Exec: exec, Req: req}, nil
	}
	return spec
}

// blockingExecutor runs until its context ends, the way a model call that is
// still streaming does, and reports what ended it.
type blockingExecutor struct {
	started chan struct{}
	events  []native.StreamEvent
}

func (e *blockingExecutor) execute(ctx context.Context, emit func(native.StreamEvent)) execEnding {
	for _, ev := range e.events {
		emit(ev)
	}
	close(e.started)
	<-ctx.Done()
	terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndCanceled}
	return execEnding{
		Terminal: &terminal,
		Phase:    phaseExecution,
		Commit: func(context.Context, *RunFailure, bool) (commitSummary, error) {
			return commitSummary{}, nil
		},
	}
}

func awaitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never started executing")
	}
}

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

	svc := newWebTestService()
	newWebTestRuntime(t, svc)
	var built sessionruntime.RunHandle
	admission, err := svc.AdmitWebRun(context.Background(), WebAdmitInput{
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
	if admission.Duplicate || admission.RunID == "" || admission.TurnID == "" || admission.Cursor.Epoch == "" {
		t.Fatalf("admission = %+v, want a started run with its turn and cursor", admission)
	}
	if built.RunID != admission.RunID || built.FencingToken <= 0 {
		t.Fatalf("admission view built for %+v, want the admitted run with its fence", built)
	}

	// Nothing has run yet: the run executes only once the entry starts it, so
	// the acceptance the client is sent cannot trail the run's own output.
	executed := make(chan struct{})
	exec := &scriptedExecutor{terminal: ptrTo(scheduleTerminalEvent(t, "done"))}
	delivery := newWebTestDelivery()
	spec := webTestSpec(svc, admission, delivery, exec)
	setup := spec.Setup
	spec.Setup = func(ctx context.Context, req ChatRequest) (runPlan, error) {
		close(executed)
		return setup(ctx, req)
	}
	select {
	case <-executed:
		t.Fatal("the run executed before its entry started it")
	default:
	}
	svc.startWebRun(admission, spec)
	if _, ended := delivery.wait(t); ended.State != string(ledger.StateCompleted) {
		t.Fatalf("ended = %+v, want completed", ended)
	}
}

// The account a Web send was made under is recorded on the run, which is what
// decides who may answer the run's approvals.
func TestAdmitWebRunRecordsTheSenderAsInitiator(t *testing.T) {
	t.Parallel()

	svc := newWebTestService()
	_, runs := newWebTestRuntime(t, svc)
	admission, err := svc.AdmitWebRun(context.Background(), WebAdmitInput{
		BotID:        webTestBotID,
		SessionID:    webTestSessionID,
		InvocationID: "invocation-1",
		InitiatorID:  "account-1",
		Payload:      []byte(`{"kind":"message","text":"hi"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	if got := ledgerRun(t, runs, admission.RunID).InitiatorID; got != "account-1" {
		t.Fatalf("run initiator = %q, want account-1", got)
	}
	delivery := newWebTestDelivery()
	svc.startWebRun(admission, webTestSpec(svc, admission, delivery, &scriptedExecutor{terminal: ptrTo(scheduleTerminalEvent(t, "done"))}))
	delivery.wait(t)
}

// A redelivered invocation names the run it already has and executes nothing.
func TestAdmitWebRunRedeliveryIsADuplicate(t *testing.T) {
	t.Parallel()

	svc := newWebTestService()
	newWebTestRuntime(t, svc)
	first := admitWebTestRun(t, svc, "invocation-1")
	delivery := newWebTestDelivery()
	svc.startWebRun(first, webTestSpec(svc, first, delivery, &scriptedExecutor{terminal: ptrTo(scheduleTerminalEvent(t, "done"))}))
	delivery.wait(t)

	again := admitWebTestRun(t, svc, "invocation-1")
	if !again.Duplicate || again.RunID != first.RunID || again.TurnID != first.TurnID {
		t.Fatalf("redelivery = %+v, want a duplicate of run %s", again, first.RunID)
	}
	if again.Cursor != (sessionruntime.Cursor{}) {
		t.Fatalf("duplicate cursor = %+v, want none: a duplicate reserved nothing", again.Cursor)
	}
}

// A session already running a turn refuses another, with the sentinel the
// client branches on.
func TestAdmitWebRunRefusesABusySession(t *testing.T) {
	t.Parallel()

	svc := newWebTestService()
	newWebTestRuntime(t, svc)
	running := admitWebTestRun(t, svc, "invocation-1")
	exec := &blockingExecutor{started: make(chan struct{})}
	delivery := newWebTestDelivery()
	svc.startWebRun(running, webTestSpec(svc, running, delivery, exec))
	awaitStarted(t, exec.started)
	t.Cleanup(func() {
		running.stop(errRunStopped)
		delivery.wait(t)
	})

	_, err := svc.AdmitWebRun(context.Background(), WebAdmitInput{
		BotID: webTestBotID, SessionID: webTestSessionID, InvocationID: "invocation-2",
		Payload: []byte(`{"kind":"message","text":"second"}`),
	})
	if !errors.Is(err, sessionruntime.ErrSessionBusy) {
		t.Fatalf("AdmitWebRun(busy) error = %v, want ErrSessionBusy", err)
	}
}

// The run's end releases the session's one active slot: the next submission
// is admitted rather than told the session is busy until a lease times out.
func TestWebRunEndReleasesTheSession(t *testing.T) {
	t.Parallel()

	svc := newWebTestService()
	_, runs := newWebTestRuntime(t, svc)
	first := admitWebTestRun(t, svc, "invocation-1")
	delivery := newWebTestDelivery()
	svc.startWebRun(first, webTestSpec(svc, first, delivery, &scriptedExecutor{terminal: ptrTo(scheduleTerminalEvent(t, "done"))}))
	delivery.wait(t)
	if got := ledgerRun(t, runs, first.RunID).State; got != ledger.StateCompleted {
		t.Fatalf("first run state = %q, want completed", got)
	}

	next := admitWebTestRun(t, svc, "invocation-2")
	if next.Duplicate || next.RunID == first.RunID {
		t.Fatalf("next admission = %+v, want a new run", next)
	}
	next.stop(errRunStopped)
	next.endActive()
}

// #1105: a Web run whose ending's history write fails must end failed, and its
// subscribers must never be shown it finishing. Publishing the terminal before
// the write is what made a failed write read as a completed turn.
func TestWebHistoryWriteFailureEndsTheRunFailedWithoutFinishing(t *testing.T) {
	t.Parallel()

	svc := newWebTestService()
	manager, runs := newWebTestRuntime(t, svc)
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

	exec := &scriptedExecutor{
		events:    []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "an answer"}},
		terminal:  ptrTo(scheduleTerminalEvent(t, "an answer")),
		commitErr: errors.New("history unavailable"),
	}
	delivery := newWebTestDelivery()
	svc.startWebRun(admission, webTestSpec(svc, admission, delivery, exec))
	events, ended := delivery.wait(t)

	run := ledgerRun(t, runs, admission.RunID)
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

	svc := newWebTestService()
	_, runs := newWebTestRuntime(t, svc)
	admission := admitWebTestRun(t, svc, "invocation-1")
	refusing := svc.publishTurnEvent
	svc.publishTurnEvent = func(ctx context.Context, handle sessionruntime.RunHandle, ev native.StreamEvent) error {
		if ev.Type == native.EventTextDelta {
			return errors.New("projection unavailable")
		}
		return refusing(ctx, handle, ev)
	}

	exec := &blockingExecutor{
		started: make(chan struct{}),
		events:  []native.StreamEvent{{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "partial"}},
	}
	delivery := newWebTestDelivery()
	svc.startWebRun(admission, webTestSpec(svc, admission, delivery, exec))
	events, _ := delivery.wait(t)

	run := ledgerRun(t, runs, admission.RunID)
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
		stop func(t *testing.T, manager *sessionruntime.Manager, admission *WebAdmission, cancelCaller context.CancelFunc)
	}{
		{
			name: "routed abort",
			stop: func(t *testing.T, manager *sessionruntime.Manager, admission *WebAdmission, _ context.CancelFunc) {
				t.Helper()
				if _, err := manager.AbortControl(context.Background(), webTestBotID, webTestSessionID, admission.RunID, "control-1"); err != nil {
					t.Fatalf("AbortControl() error = %v", err)
				}
			},
		},
		{
			name: "caller context ending",
			stop: func(_ *testing.T, _ *sessionruntime.Manager, _ *WebAdmission, cancelCaller context.CancelFunc) {
				cancelCaller()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := newWebTestService()
			manager, runs := newWebTestRuntime(t, svc)
			callerCtx, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			admission, err := svc.AdmitWebRun(callerCtx, WebAdmitInput{
				BotID: webTestBotID, SessionID: webTestSessionID, InvocationID: "invocation-1",
				Payload: []byte(`{"kind":"message","text":"hi"}`),
			})
			if err != nil {
				t.Fatalf("AdmitWebRun() error = %v", err)
			}
			exec := &blockingExecutor{started: make(chan struct{}), events: []native.StreamEvent{{Type: native.EventAgentStart}}}
			delivery := newWebTestDelivery()
			svc.startWebRun(admission, webTestSpec(svc, admission, delivery, exec))
			awaitStarted(t, exec.started)

			tc.stop(t, manager, admission, cancelCaller)
			events, ended := delivery.wait(t)

			run := ledgerRun(t, runs, admission.RunID)
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

	svc := newWebTestService()
	newWebTestRuntime(t, svc)
	svc.SetOutputPreparation(&fakeOutputMedia{}, nil)
	admission := admitWebTestRun(t, svc, "invocation-1")

	var linked []turn.OutboundAssetRef
	delivery := newWebTestDelivery()
	spec := svc.webRunSpec(admission, ChatRequest{BotID: webTestBotID, ThreadID: webTestSessionID}, delivery, inputHeld)
	spec.Setup = func(_ context.Context, req ChatRequest) (runPlan, error) {
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
		return runPlan{Exec: exec, Req: req}, nil
	}
	svc.startWebRun(admission, spec)
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

func (e *collectingExecutor) execute(_ context.Context, emit func(native.StreamEvent)) execEnding {
	for _, ev := range e.events {
		emit(ev)
	}
	terminal := native.StreamEvent{Type: native.EventAgentEnd}
	return execEnding{
		Terminal: &terminal,
		Phase:    phaseExecution,
		Commit: func(context.Context, *RunFailure, bool) (commitSummary, error) {
			e.commit()
			return commitSummary{}, nil
		},
	}
}

func ptrTo[T any](v T) *T { return &v }
