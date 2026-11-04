package application

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/runtimefence"
)

// subagentRunContext is the context AdmitSubagentRun hands a spawned run:
// its fence and its admitted handle.
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

// taskOnlyBinder arms a spawned run with its task as the only message, for
// tests about how a spawned run ends rather than what it reads.
func taskOnlyBinder(_ context.Context, task tools.SpawnRunConfig, rc *native.RunConfig, _ *tools.SubagentWatchdog) (func(context.Context) error, error) {
	rc.Messages = []sdk.Message{sdk.UserMessage(task.Query)}
	current := 0
	rc.ContextCurrentUserMessageIndex = &current
	rc.ContextQueryMaterialized = true
	return func(context.Context) error { return nil }, nil
}

// testSubagentWatchdog is a watchdog that never fires within a test.
func testSubagentWatchdog(t *testing.T) *tools.SubagentWatchdog {
	t.Helper()
	_, watchdog := tools.NewSubagentWatchdog(context.Background(), time.Hour, slog.New(slog.DiscardHandler))
	t.Cleanup(watchdog.Stop)
	return watchdog
}

// A spawned run binds only as the run admission allocated on the agent's
// thread, answering a task that is already a row of it.
func TestBindSubagentRunRequiresItsAdmittedRun(t *testing.T) {
	service := &Service{}
	admitted := subagentRunContext("bot-1", "child-1", 7)
	handle, _ := SubagentRunHandleFromContext(admitted)
	task := tools.SpawnRunConfig{
		RunID:                handle.RunID,
		Identity:             tools.SpawnIdentity{BotID: "bot-1", SessionID: "child-1", IsSubagent: true},
		TurnRequestMessageID: uuid.NewString(),
	}
	cases := []struct {
		name string
		ctx  context.Context
		task func(tools.SpawnRunConfig) tools.SpawnRunConfig
		want string
	}{
		{name: "no admission", ctx: context.Background(), task: func(t tools.SpawnRunConfig) tools.SpawnRunConfig { return t }, want: "not admitted"},
		{name: "another run", ctx: admitted, task: func(t tools.SpawnRunConfig) tools.SpawnRunConfig {
			t.RunID = uuid.NewString()
			return t
		}, want: "not the admitted one"},
		{name: "another thread", ctx: admitted, task: func(t tools.SpawnRunConfig) tools.SpawnRunConfig {
			t.Identity.SessionID = "child-2"
			return t
		}, want: "not the admitted one"},
		{name: "no request row", ctx: admitted, task: func(t tools.SpawnRunConfig) tools.SpawnRunConfig {
			t.TurnRequestMessageID = " "
			return t
		}, want: "no persisted request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg native.RunConfig
			finish, err := service.BindSubagentRun(tc.ctx, tc.task(task), &cfg, testSubagentWatchdog(t))
			if err == nil || finish != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("bind = finish %v err %v, want %q", finish != nil, err, tc.want)
			}
			if cfg.SwitchContext != nil || cfg.OnStepCommitted != nil || len(cfg.Messages) != 0 {
				t.Fatal("a refused run was armed")
			}
		})
	}
}
