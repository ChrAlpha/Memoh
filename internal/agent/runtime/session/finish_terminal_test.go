package sessionruntime

import (
	"context"
	"reflect"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
)

// sameTerminal compares two observations of one run. Cause is left out: it is
// the diagnostic the session runtime itself set, no case here ends that way,
// and comparing errors by value says nothing about the ending they name.
func sameTerminal(got, want TerminalRun) bool {
	type durable struct {
		RunID        string
		BotID        string
		SessionID    string
		FencingToken int64
		State        string
		ErrorCode    string
		Applied      bool
	}
	strip := func(r TerminalRun) durable {
		return durable{
			RunID:        r.RunID,
			BotID:        r.BotID,
			SessionID:    r.SessionID,
			FencingToken: r.FencingToken,
			State:        r.State,
			ErrorCode:    r.ErrorCode,
			Applied:      r.Applied,
		}
	}
	return strip(got) == strip(want) && reflect.DeepEqual(got.ErrorArgs, want.ErrorArgs)
}

// FinishRun hands back the same terminal row the terminal observer receives.
func TestFinishRunReturnsObservedTerminal(t *testing.T) {
	t.Parallel()
	fixture := newAdmitFixture(t)
	admission, err := fixture.manager.Admit(context.Background(), fixture.input("inv-finish-returns-terminal", `{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	var observed TerminalRun
	fixture.manager.SetTerminalObserver(func(_ context.Context, run TerminalRun) { observed = run })

	terminal, err := fixture.manager.FinishRunWithErrorCode(context.Background(), admission.Handle, RunStatusErrored, "agent.response_timeout")
	if err != nil {
		t.Fatal(err)
	}
	want := TerminalRun{
		RunID: admission.RunID, BotID: testBotID, SessionID: testSessionID,
		FencingToken: admission.Handle.FencingToken, State: string(ledger.StateFailed),
		ErrorCode: "agent.response_timeout", Applied: true,
	}
	if !sameTerminal(terminal, want) {
		t.Fatalf("FinishRunWithErrorCode() terminal = %+v, want %+v", terminal, want)
	}
	if !sameTerminal(observed, terminal) {
		t.Fatalf("observed terminal = %+v, returned %+v", observed, terminal)
	}
}

func TestFinishRunWithoutRuntimeReturnsNoTerminal(t *testing.T) {
	t.Parallel()
	var manager *Manager
	terminal, err := manager.FinishRun(context.Background(), RunHandle{}, RunStatusCompleted)
	if err != nil || !sameTerminal(terminal, TerminalRun{}) {
		t.Fatalf("FinishRun() = %+v, %v; want no terminal and no error", terminal, err)
	}
}
