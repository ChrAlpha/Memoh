package turn

import (
	"encoding/json"
	"testing"
)

func TestRunTerminalEventRoundTrip(t *testing.T) {
	payload, err := json.Marshal(RunTerminal{Type: EventRunTerminal, State: " failed ", ErrorCode: " agent.provider_overloaded "})
	if err != nil {
		t.Fatal(err)
	}
	terminal, ok := RunTerminalFrom(Event{Kind: EventRunTerminal, Payload: payload})
	if !ok || terminal.State != RunStateFailed || terminal.ErrorCode != "agent.provider_overloaded" || !terminal.Failed() {
		t.Fatalf("terminal = %+v, ok = %v", terminal, ok)
	}
}

func TestRunTerminalFromRejectsOtherEvents(t *testing.T) {
	for _, event := range []Event{
		{Kind: "error", Payload: []byte(`{"type":"run_terminal","state":"failed"}`)},
		{Kind: EventRunTerminal, Payload: []byte(`not json`)},
		{Kind: EventRunTerminal, Payload: []byte(`{"type":"run_terminal"}`)},
	} {
		if terminal, ok := RunTerminalFrom(event); ok {
			t.Fatalf("RunTerminalFrom(%s %s) = %+v", event.Kind, event.Payload, terminal)
		}
	}
}

func TestRunTerminalFailedStates(t *testing.T) {
	for state, want := range map[string]bool{
		RunStateCompleted: false,
		RunStateAborted:   false,
		RunStateFailed:    true,
		RunStateLost:      true,
	} {
		if got := (RunTerminal{State: state}).Failed(); got != want {
			t.Fatalf("Failed(%s) = %v, want %v", state, got, want)
		}
	}
}
