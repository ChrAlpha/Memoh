package persist

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
)

func TestCheckpointDistinguishesUserAbortFromRevokedOwner(t *testing.T) {
	for _, cause := range []error{context.Canceled, sessionruntime.ErrRunOwnershipLost} {
		t.Run(cause.Error(), func(t *testing.T) {
			store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
			owner, cancel := context.WithCancelCause(context.Background())
			committer := &StepCommitter{onPersisted: func() {}, writer: newTestWriter(Deps{}), persister: store, ownerContext: owner, req: runreq.ChatRequest{BotID: "bot", ThreadID: "session", RunID: "run", UserMessagePersisted: true}, rc: runreq.ResolvedContext{RunConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}}}
			cancel(cause)
			err := committer.interrupt(context.WithoutCancel(owner), 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("partial")}})
			if errors.Is(cause, sessionruntime.ErrRunOwnershipLost) {
				if !errors.Is(err, cause) || len(store.Steps) != 0 {
					t.Fatalf("revoked checkpoint: writes=%d err=%v", len(store.Steps), err)
				}
			} else if err != nil || len(store.Steps) != 1 {
				t.Fatalf("user abort checkpoint: writes=%d err=%v", len(store.Steps), err)
			}
		})
	}
}
