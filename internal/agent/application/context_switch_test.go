package application

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
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

func TestDecisionContinuationPausesIdleDuringContextSwitch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &Service{logger: slog.New(slog.DiscardHandler), messageService: &recordingMessageService{}, streamIdleTimeout: 10 * time.Second}
		var recoveryErr error
		called := false
		cfg := native.RunConfig{
			RunID: "continuation-run", ContextLifecycle: contextfrag.NewLifecycleHolder(),
			SwitchContext: func(ctx context.Context, _ native.ContextSwitchInput) (native.ContextSwitchResult, error) {
				called = true
				time.Sleep(time.Minute)
				recoveryErr = ctx.Err()
				return native.ContextSwitchResult{}, recoveryErr
			},
		}
		service.agent = native.New(native.Deps{ContextViewApplier: func(ctx context.Context, run native.RunConfig) (native.RunConfig, error) {
			_, err := run.SwitchContext(ctx, native.ContextSwitchInput{})
			if err != nil {
				return run, err
			}
			return run, contextfrag.ErrBudgetUnsatisfied
		}})
		_ = service.runNativeDecisionContinuation(t.Context(), ChatRequest{}, cfg, "", nil, make(chan WSStreamEvent, 64))
		if !called || recoveryErr != nil {
			t.Fatalf("summary was interrupted by provider idle timer: called=%v err=%v", called, recoveryErr)
		}
	})
}

func TestContextSwitchDoesNotCompactAcrossTransientInput(t *testing.T) {
	service, _, runner, cfg := chatRecoveryFixture(t)
	committer := &agentStepCommitter{service: service, ownerContext: t.Context(), req: ChatRequest{BotID: syncCompactBotID, ThreadID: syncCompactThreadID, RunID: cfg.RunID}}
	committer.bindContinuation(&cfg)
	left := switchProjectionSource(t, "left", "first")
	right := switchProjectionSource(t, "right", "second")
	input := native.ContextSwitchInput{RunID: cfg.RunID,
		Request: sdk.Request{Messages: []sdk.Message{sdk.AssistantMessage(strings.Repeat("a", 4000)), sdk.UserMessage("transient hook, absent from database"), sdk.AssistantMessage(strings.Repeat("b", 1000)), sdk.AssistantMessage("latest")}},
		Sources: []native.ContextSwitchSource{{Ref: left}, {}, {Ref: right}, {}}, Protected: []bool{false, true, false, true}, InputAllowanceTokens: 12000,
	}
	if _, err := cfg.SwitchContext(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	if len(runner.configs) != 1 {
		t.Fatalf("compaction calls = %d, want one", len(runner.configs))
	}
	if refs := runner.configs[0].EligibleSources; len(refs) != 1 || refs[0] != left {
		t.Fatalf("one summary could move evidence across a provider-only input: %+v", refs)
	}
}

func TestContextSwitchCancellationDoesNotWaitForMaintenanceGate(t *testing.T) {
	service, _, runner, cfg := chatRecoveryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	committer := &agentStepCommitter{service: service, ownerContext: ctx, req: ChatRequest{BotID: syncCompactBotID, ThreadID: syncCompactThreadID, RunID: cfg.RunID}}
	committer.bindContinuation(&cfg)
	release := service.enterSessionCompaction(syncCompactBotID, syncCompactThreadID)
	defer release()
	started := make(chan struct{})
	runner.run = func() (compaction.Result, error) {
		close(started)
		<-ctx.Done()
		return compaction.Result{}, ctx.Err()
	}
	input := native.ContextSwitchInput{RunID: cfg.RunID,
		Request: sdk.Request{Messages: []sdk.Message{sdk.UserMessage("current"), sdk.AssistantMessage(strings.Repeat("old", 2000)), sdk.AssistantMessage("latest")}},
		Sources: []native.ContextSwitchSource{{}, {Ref: switchProjectionSource(t, "old", "durable")}, {}}, Protected: []bool{true, false, true}, InputAllowanceTokens: 12000,
	}
	returned := make(chan error, 1)
	go func() { _, err := cfg.SwitchContext(ctx, input); returned <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		release()
		<-returned
		t.Fatal("run-owned recovery waited on the non-cancellable maintenance gate")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel cause = %v", err)
		}
	case <-time.After(time.Second):
		release()
		<-returned
		t.Fatal("cancelled recovery did not return while maintenance gate remained held")
	}
}
