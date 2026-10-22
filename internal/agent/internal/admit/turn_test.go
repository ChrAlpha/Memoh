package admit

import (
	"context"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// fakeTurnAdmitter captures the Admit input so tests can drive the admission
// hook exactly the way the runtime manager would at activation. Mirrors
// fakeTriggeredAdmitter in schedule_lifecycle_test.go.
type fakeTurnAdmitter struct {
	admitInput sessionruntime.AdmitInput
}

func (f *fakeTurnAdmitter) Admit(_ context.Context, input sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	f.admitInput = input
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

func (*fakeTurnAdmitter) FinishRunWithErrorCode(context.Context, sessionruntime.RunHandle, string, string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, nil
}

func (*fakeTurnAdmitter) FinishRunWithFailure(context.Context, sessionruntime.RunHandle, string, string, map[string]string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, nil
}

func (*fakeTurnAdmitter) MarkInlineDecisionRun(string, string, string) {}

func TestAdmitTurnRunProjectsRequestUserTurn(t *testing.T) {
	t.Parallel()

	admitter := &fakeTurnAdmitter{}
	svc := New(Deps{Runtime: admitter})

	cmd := turn.StartTurnCommand{
		TeamID:                  "team-1",
		Mode:                    turn.ModeChat,
		BotID:                   "bot-1",
		ThreadID:                "thread-1",
		Query:                   "hello from telegram",
		ExternalMessageID:       "ext-1",
		CurrentChannel:          "telegram",
		DisplayName:             "Alice",
		SourceChannelIdentityID: "ci-1",
	}
	admission, err := svc.Turn(context.Background(), cmd, func() {}, nil, nil)
	if err != nil {
		t.Fatalf("Turn() error = %v", err)
	}
	if admitter.admitInput.Execution.Admission == nil {
		t.Fatal("runtime admission hook is nil")
	}
	view, err := admitter.admitInput.Execution.Admission(context.Background(), admission.Handle)
	if err != nil {
		t.Fatalf("admission hook error = %v", err)
	}
	request := view.RequestUserTurn
	if request == nil {
		t.Fatal("request user turn is nil, want the inbound message projection")
	}
	if request.Text != "hello from telegram" {
		t.Fatalf("request user turn text = %q, want the inbound text", request.Text)
	}
	if request.TurnID != admission.TurnID {
		t.Fatalf("request user turn turn id = %q, want the admitted turn %q", request.TurnID, admission.TurnID)
	}
	if request.Platform != "telegram" || request.SenderDisplayName != "Alice" || request.SenderUserID != "ci-1" {
		t.Fatalf("platform/sender = (%q, %q, %q), want telegram/Alice/ci-1",
			request.Platform, request.SenderDisplayName, request.SenderUserID)
	}
}

// A channel run records who asked for it: the bound account when there is
// one, otherwise the channel identity that sent the message.
func TestAdmitTurnRunRecordsTheRequesterAsInitiator(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, userID, channelIdentityID, want string
	}{
		{"bound account", "account-1", "ci-1", "account-1"},
		{"unbound channel identity", "", "ci-1", "ci-1"},
		{"nobody", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admitter := &fakeTurnAdmitter{}
			svc := New(Deps{Runtime: admitter})
			cmd := turn.StartTurnCommand{
				TeamID: "team-1", Mode: turn.ModeChat, BotID: "bot-1", ThreadID: "thread-1", Query: "hello",
				ExternalMessageID: "ext-1", UserID: tc.userID, SourceChannelIdentityID: tc.channelIdentityID,
			}
			if _, err := svc.Turn(context.Background(), cmd, func() {}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if got := admitter.admitInput.InitiatorID; got != tc.want {
				t.Fatalf("InitiatorID = %q, want %q", got, tc.want)
			}
		})
	}
}
