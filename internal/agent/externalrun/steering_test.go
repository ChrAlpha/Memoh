package externalrun

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

func TestExternalSteerClaimsOnlyCurrentRunAndAppliesAfterConfirmation(t *testing.T) {
	service, manager, handle := newSteerHost(t)
	ctx := t.Context()
	source := service.runtimeSteering(runreq.ChatRequest{RunHandle: handle})
	if err := source.Enable(ctx); err != nil {
		t.Fatal(err)
	}
	payload, err := runreq.EncodeQueueCommand(turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: "team1", Mode: turn.ModeChat, BotID: handle.BotID, ChatID: handle.BotID,
		ThreadID: handle.SessionID, UserID: "user-1", Query: "adjust", UserVisibleText: "adjust",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.EnqueueSteer(ctx, sessionruntime.Key{BotID: handle.BotID, SessionID: handle.SessionID}, uuid.NewString(), "one", payload)
	if err != nil {
		t.Fatal(err)
	}
	item, ok, err := source.Next(ctx)
	if err != nil || !ok || item.ID != string(first.ID) || item.Text != "adjust" {
		t.Fatalf("claim: %+v %v", item, err)
	}
	replay, replayed, err := source.Next(ctx)
	if err != nil || !replayed || replay.ID != item.ID {
		t.Fatal("claim advanced before Codex confirmation")
	}
	if _, err := manager.HandleAgentEvent(ctx, handle, native.StreamEvent{Type: native.EventStepEnd, StepNumber: 0}); err != nil {
		t.Fatal(err)
	}
	if err := source.Accepted(ctx, item.ID, 0); err != nil {
		t.Fatal(err)
	}
	pending, err := manager.PendingSteers(ctx, sessionruntime.Key{BotID: handle.BotID, SessionID: handle.SessionID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatal("confirmed input remained pending")
	}
	// Round persistence replaces the provisional identity, preserving its
	// existing anchor instead of adding a second user bubble at the end.
	service.publishRuntimeSteerHistory(ctx, runreq.ChatRequest{RunHandle: handle}, []string{item.ID}, []messagepkg.Message{
		{ID: "original", TurnID: "original-turn", Role: "user", Content: []byte(`[{"type":"text","text":"adjust"}]`)},
		{ID: "steer", TurnID: "steer-turn", Role: "user", Content: []byte(`[{"type":"text","text":"adjust"}]`)},
	})
	snapshot, err := manager.Snapshot(ctx, handle.BotID, handle.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if turns := snapshot.CurrentRunView.SteerTurns; len(turns) != 1 || turns[0].TurnID != "steer-turn" || turns[0].Status != "applied" {
		t.Fatalf("durable steer identity: %+v", turns)
	}
	if err := source.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

type steerFence struct{}

func (steerFence) Activate(context.Context, string, string, int64) error { return nil }

// newSteerHost admits one run on a memory-backed runtime with steering
// enabled, and returns the host that serves it.
func newSteerHost(t *testing.T) (*Host, *sessionruntime.Manager, sessionruntime.RunHandle) {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{OwnerID: "owner-1", Ledger: sessionledger.New(), Fence: steerFence{}})
	t.Cleanup(func() { _ = manager.Close() })
	admitted, err := manager.Admit(context.Background(), sessionruntime.AdmitInput{
		BotID: "bot", SessionID: "session", InvocationID: "initial", Payload: []byte(`{}`),
		Execution: sessionruntime.Execution{Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.EnableSteer(context.Background(), admitted.Handle); err != nil {
		t.Fatal(err)
	}
	host := testHost(t, Deps{Messages: &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}, Runtime: manager})
	return host, manager, admitted.Handle
}
