package application

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/schedule"
)

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

func (*fakeTriggeredAdmitter) MarkInlineDecisionRun(string, string, string) {}

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
