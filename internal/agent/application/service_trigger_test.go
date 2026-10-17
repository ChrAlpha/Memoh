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

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/schedule"
)

// recordingTurnEventPublisher stands in for the session-runtime projection
// sink: it records every published event and can be armed to refuse the Nth
// publish, which is how fence rejections surface to the consumer.
type recordingTurnEventPublisher struct {
	mu        sync.Mutex
	events    []native.StreamEvent
	calls     int
	errAt     int // 1-based call index to fail; 0 means never fail
	err       error
	onPublish func(native.StreamEvent)
}

func (p *recordingTurnEventPublisher) publish(_ context.Context, _ sessionruntime.RunHandle, event native.StreamEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.errAt > 0 && p.calls == p.errAt {
		return p.err
	}
	if p.onPublish != nil {
		p.onPublish(event)
	}
	p.events = append(p.events, event)
	return nil
}

func (p *recordingTurnEventPublisher) published() []native.StreamEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]native.StreamEvent(nil), p.events...)
}

func scheduleTerminalEvent(t *testing.T, assistantText string) native.StreamEvent {
	t.Helper()
	messagesJSON, err := json.Marshal([]sdk.Message{sdk.AssistantMessage(assistantText)})
	if err != nil {
		t.Fatalf("marshal terminal messages: %v", err)
	}
	usageJSON, err := json.Marshal(sdk.Usage{InputTokens: 12, OutputTokens: 3})
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	return native.StreamEvent{Type: native.EventAgentEnd, Messages: messagesJSON, Usage: usageJSON}
}

func triggerStreamRequest() runreq.ChatRequest {
	return runreq.ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "run scheduled task"}
}

// fakeTriggeredAdmitter captures the Admit input so tests can drive the
// admission hook exactly the way the runtime manager would at activation.
type fakeTriggeredAdmitter struct {
	admitInput sessionruntime.AdmitInput
	admitErr   error
}

func (f *fakeTriggeredAdmitter) Admit(_ context.Context, input sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	f.admitInput = input
	if f.admitErr != nil {
		return sessionruntime.Admission{}, f.admitErr
	}
	return sessionruntime.Admission{
		Started:      true,
		RunID:        "run-1",
		TurnID:       "turn-1",
		TurnPosition: 1,
		Handle: sessionruntime.RunHandle{
			BotID:        input.BotID,
			SessionID:    input.SessionID,
			RunID:        "run-1",
			TurnID:       "turn-1",
			FencingToken: 1,
		},
	}, nil
}

func (*fakeTriggeredAdmitter) FinishRunWithErrorCode(context.Context, sessionruntime.RunHandle, string, string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, nil
}

func (*fakeTriggeredAdmitter) FinishRunWithFailure(context.Context, sessionruntime.RunHandle, string, string, map[string]string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, nil
}

func TestAdmitTriggeredRunInjectsAdmissionView(t *testing.T) {
	t.Parallel()

	admitter := &fakeTriggeredAdmitter{}
	svc := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), sessionRuntime: admitter})

	viewFn := func(handle sessionruntime.RunHandle) *sessionruntime.RunAdmissionView {
		return &sessionruntime.RunAdmissionView{
			RequestUserTurn: &chatview.UITurn{
				TurnID:    handle.TurnID,
				Role:      "user",
				Text:      "run scheduled task",
				Timestamp: time.Now(),
			},
		}
	}

	ctx, admission, _, _, err := svc.admitTriggeredRun(context.Background(), sessionmode.Schedule, "bot-1", "session-1", "inv-1", []byte(`{}`), viewFn, outcome.ErrRunStopped)
	if err != nil {
		t.Fatalf("admitTriggeredRun() error = %v", err)
	}
	if admitter.admitInput.Execution.Admission == nil {
		t.Fatal("runtime admission hook is nil")
	}
	view, err := admitter.admitInput.Execution.Admission(ctx, admission.Handle)
	if err != nil {
		t.Fatalf("admission hook error = %v", err)
	}
	if view.RequestUserTurn == nil {
		t.Fatal("request user turn is nil, want the injected projection")
	}
	if view.RequestUserTurn.Text != "run scheduled task" {
		t.Fatalf("request user turn text = %q, want the schedule command", view.RequestUserTurn.Text)
	}
	if view.RequestUserTurn.TurnID != admission.TurnID {
		t.Fatalf("request user turn turn id = %q, want the admitted turn %q", view.RequestUserTurn.TurnID, admission.TurnID)
	}
}

func TestAdmitTriggeredRunWithoutViewKeepsEmptyProjection(t *testing.T) {
	t.Parallel()

	admitter := &fakeTriggeredAdmitter{}
	svc := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), sessionRuntime: admitter})

	ctx, admission, _, _, err := svc.admitTriggeredRun(context.Background(), sessionmode.Schedule, "bot-1", "session-1", "inv-1", []byte(`{}`), nil, outcome.ErrRunStopped)
	if err != nil {
		t.Fatalf("admitTriggeredRun() error = %v", err)
	}
	view, err := admitter.admitInput.Execution.Admission(ctx, admission.Handle)
	if err != nil {
		t.Fatalf("admission hook error = %v", err)
	}
	if view.RequestUserTurn != nil {
		t.Fatalf("request user turn = %#v, want nil without a view factory", view.RequestUserTurn)
	}
}

func (*fakeTriggeredAdmitter) MarkInlineDecisionRun(string, string, string) {}

func TestScheduleInvocationUsesFireIdentity(t *testing.T) {
	a := scheduleInvocationID(schedule.TriggerPayload{ID: "schedule", SessionID: "shared", FireID: "fire-a"})
	b := scheduleInvocationID(schedule.TriggerPayload{ID: "schedule", SessionID: "shared", FireID: "fire-b"})
	if a == b {
		t.Fatal("different fires collided in the same session")
	}
	if a != scheduleInvocationID(schedule.TriggerPayload{ID: "schedule", SessionID: "shared", FireID: "fire-a"}) {
		t.Fatal("same fire lost retry identity")
	}
}

// A fire that finds the session busy reports the schedule's busy sentinel,
// which its unit treats as a skip. A failure before admission has no run
// record, so it is not marked recorded.
func TestTriggerScheduleBusyIsTheScheduleSentinel(t *testing.T) {
	t.Parallel()
	svc := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), sessionRuntime: &fakeTriggeredAdmitter{admitErr: sessionruntime.ErrSessionBusy}})

	_, err := svc.TriggerSchedule(context.Background(), "bot-1", schedule.TriggerPayload{FireID: "fire-1", ID: "schedule-1", SessionID: "session-1", Command: "run"}, "token")

	if !errors.Is(err, schedule.ErrSessionBusy) || !errors.Is(err, sessionruntime.ErrSessionBusy) {
		t.Fatalf("err = %v, want the schedule busy sentinel over the runtime's", err)
	}
	if errs.Analyze(context.Background(), err).Recorded {
		t.Fatal("a fire refused before admission must not be marked recorded")
	}
}

// A failure after admission ends the run, whose result record carries it at
// its level; the fire returns it marked recorded so its own record is a WARN.
func TestTriggerScheduleFailureAfterAdmissionIsRecorded(t *testing.T) {
	t.Parallel()
	svc := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), sessionRuntime: &fakeTriggeredAdmitter{}})

	_, err := svc.TriggerSchedule(context.Background(), "bot-1", schedule.TriggerPayload{FireID: "fire-1", ID: "schedule-1", SessionID: "session-1", Command: "run"}, "token")

	if err == nil {
		t.Fatal("TriggerSchedule() error = nil, want the run's failure")
	}
	if !errs.Analyze(context.Background(), err).Recorded {
		t.Fatalf("err = %v, want it marked recorded", err)
	}
}
