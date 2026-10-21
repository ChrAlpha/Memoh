package persist

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/runtimefence"
)

func TestAgentStepCommitterBindsContinuationToRequestMessage(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID:     handle.BotID,
		SessionID: handle.SessionID,
		Token:     handle.FencingToken,
	})
	committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
		BotID:                  handle.BotID,
		ThreadID:               handle.SessionID,
		RunID:                  handle.RunID,
		RunHandle:              handle,
		UserMessagePersisted:   true,
		PersistedUserMessageID: "request-message-id",
	}, runreq.ResolvedContext{RunConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}})
	if committer == nil {
		t.Fatal("missing agent step committer")
	}
	if _, err := committer.commit(ctx, 0, &step.Record{
		Messages: []sdk.Message{sdk.AssistantMessage("continuation response")},
	}); err != nil {
		t.Fatalf("commit continuation step: %v", err)
	}

	store := fx.messages
	if len(store.Steps) != 1 || len(store.Steps[0].Messages) != 1 {
		t.Fatalf("persisted steps = %#v, want one message", store.Steps)
	}
	if got := store.Steps[0].Messages[0].TurnRequestMessageID; got != "request-message-id" {
		t.Fatalf("turn request message ID = %q, want %q", got, "request-message-id")
	}
}
