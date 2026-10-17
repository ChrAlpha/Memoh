package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// A replacement run without step commits has no queue coordinator to finalize
// it: the ending writes the new turn from the terminal snapshot, and the
// replacement's PostPersist then rewrites the old turn, once.
func TestNativeReplacementWithoutStepCommitsRewritesTheTurnOnce(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	service := wireComponents(&Service{logger: slog.New(slog.DiscardHandler), messageService: messages})
	position := int64(2)
	req := continuationRequest()
	req.ReusePersistedUserMessage = true
	req.TurnID = "new-turn"
	req.TurnPosition = &position
	replace := &webReplacement{service: service, reason: "retry", oldTurnID: "old-turn"}
	req = replace.target(context.Background(), req, req.PersistedUserMessageID)
	snapshot, err := json.Marshal([]sdk.Message{sdk.AssistantMessage("ANSWER")})
	if err != nil {
		t.Fatal(err)
	}
	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventTextDelta, Delta: "ANSWER"},
			{Type: native.EventAgentEnd, Messages: snapshot, EndReason: event.EndCompleted},
		},
		req:     req,
		timing:  persist.NewReasoningTimingTracker(nil),
		replace: replace.hooks(),
		service: service,
	}

	ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
	if _, err := ending.Commit(context.Background(), nil, false); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if len(messages.Persisted) == 0 {
		t.Fatal("the terminal snapshot was not written")
	}
	if messages.Replaced != 1 || messages.ReplacementTurnID != "new-turn" {
		t.Fatalf("ReplaceTurn calls = %d to turn %q, want one to new-turn", messages.Replaced, messages.ReplacementTurnID)
	}
}

// The native agent writes the step it was in when a stop arrived, as the stop
// unwinds the stream, so its ending has nothing left for the driver's stopped
// write. A hook here would write that step a second time.
func TestNativeExecutorLeavesTheStoppedWriteToTheRuntime(t *testing.T) {
	t.Parallel()

	exec := &nativeExecutor{
		agent: fixedStream{
			{Type: native.EventAgentStart},
			{Type: native.EventTextDelta, Delta: "partial"},
			{Type: native.EventAgentAbort, EndReason: event.EndCanceled},
		},
		timing:  persist.NewReasoningTimingTracker(nil),
		service: wireComponents(&Service{logger: slog.New(slog.DiscardHandler)}),
	}

	ending := exec.Execute(context.Background(), func(native.StreamEvent) {})
	if ending.CommitStopped != nil {
		t.Fatal("the native ending offers a stopped write, want the runtime's own step checkpoint to be the only one")
	}
}
