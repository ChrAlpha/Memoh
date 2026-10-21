package externalrun

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// A decision projection is filed under the run's request row. Without one the
// row is not written, instead of landing in whichever turn is latest.
func TestDecisionProjectionNeedsARequestRow(t *testing.T) {
	t.Parallel()
	messages := &agenttest.RecordingMessages{}
	host := testHost(t, Deps{Messages: messages})
	ev := native.StreamEvent{
		Type: native.EventToolApprovalRequest, ToolName: "exec", ToolCallID: "call-1", ApprovalID: "approval-1",
		Input: map[string]any{"command": "ls"},
	}
	req := runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1", RunID: "run-1"}

	if got := host.persistRuntimeDecisionProjection(t.Context(), req, ev); got != nil || len(messages.Persisted) != 0 {
		t.Fatalf("projection without a request row = %v, persisted %d rows, want none", got, len(messages.Persisted))
	}

	req.PersistedUserMessageID = "request-1"
	if got := host.persistRuntimeDecisionProjection(t.Context(), req, ev); got == nil || len(messages.Persisted) != 1 {
		t.Fatalf("projection with a request row = %v, persisted %d rows, want one", got, len(messages.Persisted))
	}
	if input := messages.Persisted[0]; input.TurnRequestMessageID != "request-1" || input.RunID != "run-1" {
		t.Fatalf("projection filed under request %q for run %q", input.TurnRequestMessageID, input.RunID)
	}
}
