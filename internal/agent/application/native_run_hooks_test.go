package application

import (
	"context"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/runtimefence"
)

// bindNativeRun declares a run's hooks from its request: every admitted turn
// that writes visible rows folds in-run, steer is armed only for queue-enabled
// turns, and a replacement turn, whose rows stay hidden until it ends, never
// folds.
func TestBindNativeRunDeclaresHooksByRequest(t *testing.T) {
	service, handle := newDeferredSteerTestService(t)
	service.compactionService = compaction.NewService(slog.New(slog.DiscardHandler), nil)
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{BotID: handle.BotID, SessionID: handle.SessionID, Token: handle.FencingToken})
	rc := resolvedContext{runConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}}
	position := int64(1)
	base := ChatRequest{BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle, TurnID: "turn", TurnPosition: &position}
	continuation := base
	continuation.QueueSteerEnabled, continuation.UserMessagePersisted, continuation.PersistedUserMessageID = true, true, "user"
	for _, tc := range []struct {
		name       string
		req        ChatRequest
		wantSwitch bool
		wantSteer  bool
	}{
		{name: "admitted turn", req: base, wantSwitch: true},
		{name: "queue continuation", req: continuation, wantSwitch: true, wantSteer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg native.RunConfig
			if committer := service.bindNativeRun(ctx, tc.req, rc, &cfg, nil); committer == nil || cfg.OnStepCommitted == nil {
				t.Fatal("the run was not armed for step persistence")
			}
			if (cfg.SwitchContext != nil) != tc.wantSwitch || (cfg.OnSteer != nil) != tc.wantSteer || (cfg.SteerWake != nil) != tc.wantSteer {
				t.Fatalf("switch=%v steer=%v wake=%v, want switch=%v steer=%v", cfg.SwitchContext != nil, cfg.OnSteer != nil, cfg.SteerWake != nil, tc.wantSwitch, tc.wantSteer)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		committer *agentStepCommitter
	}{
		{name: "replacement turn", committer: &agentStepCommitter{service: service, req: ChatRequest{TurnReplacement: &messagepkg.TurnReplacement{}}}},
		{name: "no compaction service", committer: &agentStepCommitter{service: &Service{}, req: base}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg native.RunConfig
			tc.committer.bindContinuation(&cfg)
			if cfg.SwitchContext != nil {
				t.Fatal("the run was armed to fold")
			}
		})
	}
}
