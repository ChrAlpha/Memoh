package application

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
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
	spec := service.webRunSpec(admission, ChatRequest{BotID: "b", ThreadID: "s"}, nil, inputHeld)
	spec.Setup = func(context.Context, ChatRequest) (runPlan, error) {
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

		if _, err := streamWebSend(context.Background(), t, service, ChatRequest{BotID: "b", ThreadID: "s"}); err == nil {
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
		spec := service.webRunSpec(admission, ChatRequest{BotID: "b", ThreadID: "s"}, newWebTestDelivery(), inputHeld)
		spec.Setup = func(_ context.Context, req ChatRequest) (runPlan, error) {
			return runPlan{Exec: exec, Req: req}, nil
		}
		service.startWebRun(admission, spec)
		awaitStarted(t, exec.started)

		admission.stop(errRunStopped)

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

	if _, err := streamWebSend(context.Background(), t, service, ChatRequest{BotID: "b", ThreadID: "s"}); err == nil {
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
		service.sessionRuntime = newScriptedAdmitter()
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

// The external-runtime path used to write the error channel directly, which
// meant the caller was told the turn failed while the span said it completed.
// A trace that disagrees with the answer the caller got is worse than a trace
// with nothing in it.
func TestTurnSpanReportsFailureFromEveryPath(t *testing.T) {
	recorder := recordTurnSpans(t)
	service := newTracingTestService()

	_, callerSaw := streamWebSend(context.Background(), t, service, ChatRequest{BotID: "b", ThreadID: "s"})

	span := turnSpan(t, recorder)
	outcome := spanAttr(span, "agent.turn.outcome").AsString()
	switch {
	case callerSaw != nil && outcome == "completed":
		t.Fatalf("caller was told %v but the span says completed", callerSaw)
	case callerSaw == nil && outcome == "errored":
		t.Fatal("the span says errored but the caller was told nothing")
	}
}
