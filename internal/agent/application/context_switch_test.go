package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func TestStepCommitterBindsBoundedContextSwitch(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_progress", true: "cancelled"}[cancelled], func(t *testing.T) {
			service, _, runner, cfg := chatRecoveryFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			committer := &agentStepCommitter{service: service, ownerContext: ctx, req: ChatRequest{BotID: syncCompactBotID, ThreadID: syncCompactThreadID, RunID: cfg.RunID}, nextStep: 2}
			committer.bindContinuation(&cfg)
			if cfg.SwitchContext == nil {
				t.Fatal("durable run has no context-switch path")
			}
			runner.run = func() (compaction.Result, error) {
				if cancelled {
					cancel()
				}
				return compaction.Result{Status: compaction.StatusOK}, nil
			}
			input := native.ContextSwitchInput{RunID: cfg.RunID, StepIndex: 2,
				Request:   sdk.Request{Messages: []sdk.Message{sdk.UserMessage("current"), sdk.AssistantMessage(strings.Repeat("old", 2000)), sdk.AssistantMessage("latest")}},
				Sources:   []native.ContextSwitchSource{{}, {Ref: switchProjectionSource(t, "old", "durable")}, {}},
				Protected: []bool{true, false, true}, InputAllowanceTokens: 12000,
			}
			got, err := cfg.SwitchContext(ctx, input)
			if got.Messages != nil || len(runner.configs) != 1 {
				t.Fatalf("unchanged artifact looped or was adopted: candidate=%+v calls=%d err=%v", got, len(runner.configs), err)
			}
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost after compaction: %v", err)
			}
			if !cancelled && err != nil {
				t.Fatal(err)
			}
			if got := runner.configs[0].EligibleSources; len(got) != 1 || got[0] != input.Sources[1].Ref {
				t.Fatalf("compaction scope changed: %+v", got)
			}
		})
	}
}
