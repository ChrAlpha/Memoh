package application

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
)

func TestCheckpointDistinguishesUserAbortFromRevokedOwner(t *testing.T) {
	for _, cause := range []error{context.Canceled, sessionruntime.ErrRunOwnershipLost} {
		t.Run(cause.Error(), func(t *testing.T) {
			store := &recordingStepPersister{recordingMessageService: &recordingMessageService{}}
			owner, cancel := context.WithCancelCause(context.Background())
			committer := &agentStepCommitter{service: &Service{}, persister: store, ownerContext: owner, req: ChatRequest{BotID: "bot", ThreadID: "session", RunID: "run", UserMessagePersisted: true}, rc: resolvedContext{runConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}}}
			cancel(cause)
			err := committer.interrupt(context.WithoutCancel(owner), 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("partial")}})
			if errors.Is(cause, sessionruntime.ErrRunOwnershipLost) {
				if !errors.Is(err, cause) || len(store.steps) != 0 {
					t.Fatalf("revoked checkpoint: writes=%d err=%v", len(store.steps), err)
				}
			} else if err != nil || len(store.steps) != 1 {
				t.Fatalf("user abort checkpoint: writes=%d err=%v", len(store.steps), err)
			}
		})
	}
}

func TestSubagentRejectsRevokedCheckpointWithDetachedCallback(t *testing.T) {
	service, store, ctx, task := newChildBindingFixture(t, 0)
	owner, cancel := context.WithCancelCause(ctx)
	cfg := native.RunConfig{RunID: task.RunID, ContextLifecycle: contextfrag.NewLifecycleHolder()}
	_, err := service.BindSubagentNativeRun(owner, task, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	cancel(sessionruntime.ErrRunOwnershipLost)
	err = cfg.OnStepInterrupted(context.WithoutCancel(owner), 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("late")}})
	if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) || len(store.steps) != 0 {
		t.Fatalf("revoked child wrote %d steps: %v", len(store.steps), err)
	}
}
