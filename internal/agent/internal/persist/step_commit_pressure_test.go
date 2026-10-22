package persist

import (
	"context"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/runtimefence"
)

// Every committed step is context the next request carries.
func TestStepCommitterCountsCommittedStepsIntoTheRunPressure(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{BotID: handle.BotID, SessionID: handle.SessionID, Token: handle.FencingToken})
	req := runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle,
		UserMessagePersisted: true, PersistedUserMessageID: "user",
	}
	pressure := runreq.NewRunPressure(nil, nil)
	committer := fx.writer.NewStepCommitter(ctx, req, runreq.ResolvedContext{
		RunConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()},
		Pressure:  pressure,
	})
	if committer == nil {
		t.Fatal("missing fenced step committer")
	}
	stepMessages := []sdk.Message{sdk.AssistantMessage("a long answer the next request carries")}
	if _, err := committer.commit(ctx, 0, &step.Record{Messages: stepMessages}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got, want := pressure.Tokens(), runreq.RequestInputTokens(0, stepMessages); got != want {
		t.Fatalf("run pressure = %d, want the committed step's %d", got, want)
	}
}
