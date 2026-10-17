package application

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
)

func TestSubagentRejectsRevokedCheckpointWithDetachedCallback(t *testing.T) {
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	service := wireComponents(&Service{messageService: store})
	owner, cancel := context.WithCancelCause(subagentRunContext("bot", "session", 9))
	_, checkpoint := service.SubagentStepCommit(owner, "bot", "session", "model", "request", nil, nil)
	if checkpoint == nil {
		t.Fatal("missing checkpoint callback")
	}
	cancel(sessionruntime.ErrRunOwnershipLost)
	err := checkpoint(context.WithoutCancel(owner), 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("late")}})
	if !errors.Is(err, sessionruntime.ErrRunOwnershipLost) || len(store.Steps) != 0 {
		t.Fatalf("revoked subagent checkpoint: writes=%d err=%v", len(store.Steps), err)
	}
}
