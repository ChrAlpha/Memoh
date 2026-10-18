package agent

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

func TestWebContinuationDoesNotCreateChannelCheckpoint(t *testing.T) {
	backend := sessionruntime.NewMemoryBackend()
	manager, handle := newWaitingDecisionRuntime(t, backend)
	service := continuationService(t, manager)
	cmd := sessionruntime.Command{ID: "web-answer", BotID: handle.BotID, SessionID: handle.SessionID, RunID: handle.RunID, Generation: handle.Generation}
	// Web calls RouteDecisionResponse directly; it never opens StreamDecisionResponse.
	continueDecision(t, service, manager, cmd, scriptedRun([]native.StreamEvent{
		{Type: native.EventAgentStart}, {Type: native.EventTextDelta, Delta: "web reply"}, {Type: native.EventAgentEnd},
	}, nil))
	page, err := backend.ReadDecisionOutput(context.Background(), sessionruntime.DecisionOutputRef{BotID: handle.BotID, CommandID: cmd.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Snapshot(context.Background(), handle.BotID, handle.SessionID)
	if err != nil || live.CurrentRunView == nil || live.CurrentRunView.Status != sessionruntime.RunStatusCompleted {
		t.Fatalf("web continuation did not finish: %+v, %v", live.CurrentRunView, err)
	}
	if page.Exists {
		t.Fatalf("Web-only continuation retained %d channel events without a channel subscription", page.Length)
	}
}
