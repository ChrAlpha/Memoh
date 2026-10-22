package admit

import (
	"context"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/internal/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
)

func TestAdmitTriggeredRunInjectsAdmissionView(t *testing.T) {
	t.Parallel()

	admitter := &fakeTurnAdmitter{}
	a := New(Deps{Runtime: admitter})

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

	ctx, admission, _, err := a.Triggered(context.Background(), Trigger{BotID: "bot-1", ThreadID: "session-1", InvocationID: "inv-1", Submission: []byte(`{}`), View: viewFn, StopCause: outcome.ErrRunStopped})
	if err != nil {
		t.Fatalf("Triggered() error = %v", err)
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

	admitter := &fakeTurnAdmitter{}
	a := New(Deps{Runtime: admitter})

	ctx, admission, _, err := a.Triggered(context.Background(), Trigger{BotID: "bot-1", ThreadID: "session-1", InvocationID: "inv-1", Submission: []byte(`{}`), StopCause: outcome.ErrRunStopped})
	if err != nil {
		t.Fatalf("Triggered() error = %v", err)
	}
	view, err := admitter.admitInput.Execution.Admission(ctx, admission.Handle)
	if err != nil {
		t.Fatalf("admission hook error = %v", err)
	}
	if view.RequestUserTurn != nil {
		t.Fatalf("request user turn = %#v, want nil without a view factory", view.RequestUserTurn)
	}
}
