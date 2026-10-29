package persist

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
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

// The run's first step records on its request row the runtime context the
// request carried, and the committer reports it from then on. A run that
// carries none, such as one resumed after a decision, leaves what the row
// recorded alone.
func TestAgentStepCommitterReportsTheRecordedRuntimeContext(t *testing.T) {
	cases := []struct {
		name    string
		context []native.TurnContext
		want    bool
	}{
		{"carried", []native.TurnContext{{Kind: messagepkg.RuntimeContextMemoryRecall, Message: sdk.UserMessage("recalled memory")}}, true},
		{"none carried", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSteerFixture(t)
			handle := fx.handle
			ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{BotID: handle.BotID, SessionID: handle.SessionID, Token: handle.FencingToken})
			committer := fx.writer.NewStepCommitter(ctx, runreq.ChatRequest{
				BotID:                  handle.BotID,
				ThreadID:               handle.SessionID,
				RunID:                  handle.RunID,
				RunHandle:              handle,
				UserMessagePersisted:   true,
				PersistedUserMessageID: "request-message-id",
			}, runreq.ResolvedContext{RunConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder(), ContextTurn: tc.context}})
			if committer.RuntimeContextRecorded() {
				t.Fatal("the runtime context is reported recorded before any step")
			}
			for i, text := range []string{"first step", "second step"} {
				if _, err := committer.commit(ctx, i, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage(text)}}); err != nil {
					t.Fatalf("commit step %d: %v", i, err)
				}
			}

			if got := committer.RuntimeContextRecorded(); got != tc.want {
				t.Fatalf("recorded = %t, want %t", got, tc.want)
			}
			steps := fx.messages.Steps
			if len(steps) != 2 || (len(steps[0].RequestMetadata) > 0) != tc.want || len(steps[1].RequestMetadata) > 0 {
				t.Fatalf("request metadata by step = %v, want it on the first step only when the run carries context", steps)
			}
		})
	}
}
