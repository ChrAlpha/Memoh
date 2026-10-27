package background

import (
	"encoding/json"
	"testing"
)

func TestAgentTaskEventPreservesSessionStringOnTheWire(t *testing.T) {
	raw, err := json.Marshal(TaskEvent{Event: TaskEventStarted, AgentSessionID: "child"})
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data["agent_session_id"] != "child" || data["AgentSessionID"] != nil {
		t.Fatalf("task event broke the session link: %s", raw)
	}
}
