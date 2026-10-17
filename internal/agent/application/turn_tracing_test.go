package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func recordTurnSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return recorder
}

func turnSpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, span := range recorder.Ended() {
			if span.Name() == "agent.turn" {
				return span
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no agent.turn span ended within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func spanAttr(span sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// The span covers the turn, not the moment it is set up. A span closed
// before the work runs would report microseconds for a turn that took a
// minute — worse than no span, because it looks like an answer.
//
// The assertion is made while the turn is provably still running, by holding
// it inside its setup. Checking "has it ended yet" straight after the call
// would only be a race with the goroutine's scheduling.
func TestTurnSpanCoversTheTurnNotItsSetup(t *testing.T) {
	recorder := recordTurnSpans(t)
	service := newTracingTestService()

	inTurn := make(chan struct{})
	release := make(chan struct{})
	admission := admitTracingWebRun(t, service)
	spec := service.webRunSpec(admission, runreq.ChatRequest{BotID: "b", ThreadID: "s"}, nil, outcome.InputHeld)
	spec.Setup = func(context.Context, runreq.ChatRequest) (runPlan, error) {
		close(inTurn)
		<-release
		return runPlan{}, errors.New("setup ends the turn")
	}
	service.startWebRun(admission, spec)

	select {
	case <-inTurn:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never reached the preflight hook")
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "agent.turn" {
			t.Fatal("the turn span ended while the turn was still running")
		}
	}
	close(release)

	span := turnSpan(t, recorder)
	if got := spanAttr(span, "agent.bot_id").AsString(); got != "b" {
		t.Errorf("agent.bot_id = %q, want b", got)
	}
	if got := spanAttr(span, "agent.thread_id").AsString(); got != "s" {
		t.Errorf("agent.thread_id = %q, want s", got)
	}
	if got := span.SpanKind(); got != trace.SpanKindInternal {
		t.Errorf("span kind = %v, want internal", got)
	}
}

// A turn that failed is marked; a turn someone stopped is not. Recording
// cancellations as errors would make the error rate track how often users
// press stop.
func TestTurnSpanSeparatesFailureFromCancellation(t *testing.T) {
	t.Run("errored", func(t *testing.T) {
		recorder := recordTurnSpans(t)
		service := newTracingTestService()

		if _, err := streamWebSend(context.Background(), t, service, runreq.ChatRequest{BotID: "b", ThreadID: "s"}); err == nil {
			t.Fatal("expected this harness to fail the turn")
		}

		span := turnSpan(t, recorder)
		if got := spanAttr(span, "agent.turn.outcome").AsString(); got != "errored" {
			t.Errorf("outcome = %q, want errored", got)
		}
		if span.Status().Code != codes.Error {
			t.Error("a failed turn did not mark the span as an error")
		}
	})

	t.Run("aborted", func(t *testing.T) {
		recorder := recordTurnSpans(t)
		service := newTracingTestService()
		admission := admitTracingWebRun(t, service)
		exec := &blockingExecutor{started: make(chan struct{})}
		spec := service.webRunSpec(admission, runreq.ChatRequest{BotID: "b", ThreadID: "s"}, newWebTestDelivery(), outcome.InputHeld)
		spec.Setup = func(_ context.Context, req runreq.ChatRequest) (runPlan, error) {
			return runPlan{Exec: exec, Req: req}, nil
		}
		service.startWebRun(admission, spec)
		awaitStarted(t, exec.started)

		admission.stop(outcome.ErrRunStopped)

		span := turnSpan(t, recorder)
		if got := spanAttr(span, "agent.turn.outcome").AsString(); got != "aborted" {
			t.Errorf("outcome = %q, want aborted", got)
		}
		if span.Status().Code == codes.Error {
			t.Error("a stopped turn was recorded as a failure")
		}
	})
}

// The web UI's WebSocket never goes through the turn port, so a span placed
// only on StartTurn would be missing for the entry point most turns arrive
// through — and nothing would say so; the traces would simply have no turn in
// them. This is the second entry point, asserted separately for that reason.
func TestTurnSpanCoversTheWebSocketEntryPoint(t *testing.T) {
	recorder := recordTurnSpans(t)
	service := newTracingTestService()

	if _, err := streamWebSend(context.Background(), t, service, runreq.ChatRequest{BotID: "b", ThreadID: "s"}); err == nil {
		t.Fatal("expected this harness to fail the turn")
	}

	span := turnSpan(t, recorder)
	if got := spanAttr(span, "agent.bot_id").AsString(); got != "b" {
		t.Errorf("agent.bot_id = %q, want b", got)
	}
	if span.Status().Code != codes.Error {
		t.Error("a turn that failed was not marked as an error")
	}
}

// admitTracingWebRun admits a Web run on bot b, session s.
func admitTracingWebRun(t *testing.T, service *Service) *WebAdmission {
	t.Helper()
	if service.sessionRuntime == nil {
		service.sessionRuntime = agenttest.NewScriptedAdmitter()
	}
	admission, err := service.AdmitWebRun(context.Background(), WebAdmitInput{
		BotID: "b", SessionID: "s", InvocationID: "trace-1", Payload: []byte(`{"kind":"message"}`),
	})
	if err != nil {
		t.Fatalf("AdmitWebRun() error = %v", err)
	}
	return admission
}

// newTracingTestService is newTurnTestService plus a logger. The harness
// leaves it nil because the paths it was written for never log; these tests
// drive runs that do.
func newTracingTestService() *Service {
	service := newTurnTestService()
	service.logger = slog.New(slog.DiscardHandler)
	return service
}

// The session runtime cancels a run's context once the run's terminal state is
// written. The span closes after that, so it must not read how the run ended
// from the context: every run would then look cancelled.
func TestTurnSpanOutcomeSurvivesTheSessionRuntimeCancellingTheRun(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		status  codes.Code
		exec    func() (runExecutor, <-chan struct{})
	}{
		{name: "completed", outcome: "completed", status: codes.Unset, exec: func() (runExecutor, <-chan struct{}) {
			terminal := native.StreamEvent{Type: native.EventAgentEnd}
			return &scriptedExecutor{terminal: &terminal}, nil
		}},
		{name: "failed", outcome: "errored", status: codes.Error, exec: func() (runExecutor, <-chan struct{}) {
			terminal := native.StreamEvent{Type: native.EventAgentAbort, EndReason: event.EndFailed}
			return &scriptedExecutor{terminal: &terminal}, nil
		}},
		{name: "stopped", outcome: "aborted", status: codes.Unset, exec: func() (runExecutor, <-chan struct{}) {
			exec := &blockingExecutor{started: make(chan struct{})}
			return exec, exec.started
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := recordTurnSpans(t)
			svc := newWebTestService()
			newWebTestRuntime(t, svc)
			admission := admitWebTestRun(t, svc, "span-"+tc.name)
			exec, started := tc.exec()
			delivery := newWebTestDelivery()
			svc.startWebRun(admission, webTestSpec(svc, admission, delivery, exec))
			if started != nil {
				awaitStarted(t, started)
				admission.stop(outcome.ErrRunStopped)
			}
			delivery.wait(t)

			span := turnSpan(t, recorder)
			if got := spanAttr(span, "agent.turn.outcome").AsString(); got != tc.outcome {
				t.Errorf("outcome = %q, want %q", got, tc.outcome)
			}
			if got := span.Status().Code; got != tc.status {
				t.Errorf("status = %v, want %v", got, tc.status)
			}
		})
	}
}

// spanTestDriver is the External Agent runtime the entry table runs every entry
// on. The run it gives each entry ends one of three ways: it completes, its
// prompt fails, or it holds the turn open until the entry stops it.
type spanTestDriver struct {
	outcome string
	started chan struct{}
	once    sync.Once
}

func (*spanTestDriver) RuntimeType() string { return session.RuntimeACPAgent }

func (d *spanTestDriver) Prompt(ctx context.Context, _ external.PromptInput) (external.PromptResult, error) {
	switch d.outcome {
	case "failed":
		return external.PromptResult{}, errors.New("the runtime refused the prompt")
	case "stopped":
		d.once.Do(func() { close(d.started) })
		<-ctx.Done()
		return external.PromptResult{Text: "partial"}, ctx.Err()
	default:
		return external.PromptResult{TurnCompleted: true, Text: "done", Output: []sdk.Message{sdk.AssistantMessage("done")}}, nil
	}
}

// spanTestEntry starts one entry's run and says how to stop it and how to wait
// for it. An entry that runs on the caller's goroutine returns when the run
// ends; the others hand back what they were given.
type spanTestEntry struct {
	name  string
	start func(t *testing.T, service *Service) (stop func(), wait func())
}

// awaitEntryReturned waits for an entry that runs on its caller's goroutine to
// return, which it does once its run has ended.
func awaitEntryReturned(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the entry did not return after its run")
	}
}

func spanTestEntries() []spanTestEntry {
	return []spanTestEntry{
		{name: "web_send", start: func(t *testing.T, service *Service) (func(), func()) {
			admission, err := service.AdmitWebRun(context.Background(), WebAdmitInput{
				BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
				InvocationID: "span-web", Payload: []byte(`{"kind":"message"}`),
			})
			if err != nil {
				t.Fatalf("AdmitWebRun() error = %v", err)
			}
			delivery := newWebTestDelivery()
			service.RunWebSend(admission, runreq.ChatRequest{
				BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
				Query: directLifecyclePrompt,
			}, delivery) //nolint:contextcheck // The run's context is the one admission derived from ctx.
			return func() { admission.stop(outcome.ErrRunStopped) }, func() { delivery.wait(t) }
		}},
		{name: "im_start_turn", start: func(t *testing.T, service *Service) (func(), func()) {
			handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{
				SchemaVersion: 1, TeamID: driveTeamID, Mode: turn.ModeChat,
				BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
				Query: directLifecyclePrompt, ExternalMessageID: "span-im", SourceChannelIdentityID: "im-sender-1",
				UserID: "user-1", CurrentChannel: "telegram", Channels: []string{"telegram"},
				ReplyTarget: "im-chat-1", ConversationType: "private",
			})
			if err != nil {
				t.Fatalf("StartTurn() error = %v", err)
			}
			return handle.Cancel, func() { drainDriveHandle(t, handle) }
		}},
		{name: "schedule", start: func(t *testing.T, service *Service) (func(), func()) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = service.TriggerSchedule(context.Background(), agenttest.LifecycleBotID, driveSchedulePayload("user-1"), "")
			}()
			// A routed abort reaches the fire through the cancel the admission
			// registered.
			admitter := service.sessionRuntime.(*agenttest.LifecycleAdmitter)
			return func() { admitter.Cancel() }, func() { awaitEntryReturned(t, done) }
		}},
		{name: "session_resume", start: func(t *testing.T, service *Service) (func(), func()) {
			const teamID = "00000000-0000-0000-0000-000000000001"
			setAllowedTeam(service, teamID)
			raw, err := json.Marshal(map[string]any{"resume": resumeContext{
				Version: 1, ChatID: agenttest.LifecycleBotID, UserID: "user-1", Query: directLifecyclePrompt, SessionType: sessionmode.Chat,
			}})
			if err != nil {
				t.Fatal(err)
			}
			done, err := service.resumeInterruptedSession(t.Context(), sqlc.SessionRun{
				TeamID:    db.ParseUUIDOrEmpty(teamID),
				RunID:     db.ParseUUIDOrEmpty(uuid.NewString()),
				BotID:     db.ParseUUIDOrEmpty(agenttest.LifecycleBotID),
				SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID),
				InputJson: raw,
			})
			if err != nil {
				t.Fatalf("resumeInterruptedSession() error = %v", err)
			}
			admitter := service.sessionRuntime.(*agenttest.LifecycleAdmitter)
			return func() { admitter.Cancel() }, func() { awaitEntryReturned(t, done) }
		}},
		{name: "decision_continuation", start: func(t *testing.T, service *Service) (func(), func()) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = service.driveUnroutedContinuation(ctx, decisionContinuation{
					Req: runreq.ChatRequest{BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID, Query: directLifecyclePrompt},
					Setup: func(ctx context.Context, req runreq.ChatRequest) (runPlan, error) {
						return service.externalRunPlan(ctx, req, runSpec{Input: outcome.InputAccepted}, service.externalDrivers[session.RuntimeACPAgent])
					},
				}, nil)
			}()
			return cancel, func() { awaitEntryReturned(t, done) }
		}},
	}
}

// Every entry runs through the driver, so every entry has a turn span: exactly
// one, and one that says how the run ended. A run that failed is an error on the
// span, a run someone stopped is not, and the span of a run that completed says
// so. The trace has to agree with what the caller was told, or it is worse than
// no trace.
func TestTurnSpanReportsFailureFromEveryPath(t *testing.T) {
	want := map[string]struct {
		outcome string
		status  codes.Code
	}{
		"completed": {outcome: "completed", status: codes.Unset},
		"failed":    {outcome: "errored", status: codes.Error},
		"stopped":   {outcome: "aborted", status: codes.Unset},
	}
	for _, entry := range spanTestEntries() {
		for _, ending := range []string{"completed", "failed", "stopped"} {
			t.Run(entry.name+"/"+ending, func(t *testing.T) {
				recorder := recordTurnSpans(t)
				driver := &spanTestDriver{outcome: ending, started: make(chan struct{})}
				service := newACPLifecycleService(t, &recordingACPPrompter{}, &agenttest.RecordingMessages{}, &agenttest.RecordingLifecycleStore{})
				service.sessionRuntime = &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission()}
				registerExternalDrivers(service, driver)

				stop, wait := entry.start(t, service)
				if ending == "stopped" {
					awaitStarted(t, driver.started)
					stop()
				}
				wait()

				span := turnSpan(t, recorder)
				// A second span, from a second layer that also opens one, would
				// end alongside the first.
				time.Sleep(100 * time.Millisecond)
				var spans int
				for _, ended := range recorder.Ended() {
					if ended.Name() == "agent.turn" {
						spans++
					}
				}
				if spans != 1 {
					t.Fatalf("agent.turn spans = %d, want exactly one", spans)
				}
				if got := spanAttr(span, "agent.turn.outcome").AsString(); got != want[ending].outcome {
					t.Errorf("outcome = %q, want %q", got, want[ending].outcome)
				}
				if got := span.Status().Code; got != want[ending].status {
					t.Errorf("status = %v, want %v", got, want[ending].status)
				}
				if got := spanAttr(span, "agent.bot_id").AsString(); got != agenttest.LifecycleBotID {
					t.Errorf("agent.bot_id = %q, want %q", got, agenttest.LifecycleBotID)
				}
			})
		}
	}
}
