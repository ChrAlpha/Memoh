package application

import (
	"context"
	"testing"

	"github.com/google/uuid"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/runtimefence"
)

func subagentRunContext(botID, sessionID string, token int64) context.Context {
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID: botID, SessionID: sessionID, Token: token,
	})
	return withSubagentRunHandle(ctx, sessionruntime.RunHandle{
		BotID:        botID,
		SessionID:    sessionID,
		RunID:        uuid.NewString(),
		TurnID:       uuid.NewString(),
		Generation:   "gen-1",
		FencingToken: token,
	})
}

func TestSubagentRunObserverRequiresRuntimeAndHandle(t *testing.T) {
	service := &Service{}
	if got := service.SubagentRunObserver(subagentRunContext(uuid.NewString(), uuid.NewString(), 5)); got != nil {
		t.Fatal("observer enabled without a session runtime")
	}
	withRuntime := &Service{decisionRuntime: &sessionruntime.Manager{}}
	if got := withRuntime.SubagentRunObserver(context.Background()); got != nil {
		t.Fatal("observer enabled without an admitted run handle")
	}
}
