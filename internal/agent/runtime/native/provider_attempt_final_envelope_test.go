package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agenttools "github.com/memohai/memoh/internal/agent/tool"
	"github.com/memohai/memoh/internal/models"
)

func oversizedPrefixRunConfig(provider sdk.Provider, plan *contextfrag.ContextBudgetPlan) RunConfig {
	return RunConfig{
		Model:                  &sdk.Model{ID: "mock-model", Provider: provider},
		System:                 "system",
		Messages:               []sdk.Message{sdk.UserMessage(strings.Repeat("oversized ", 2_000))},
		Identity:               SessionContext{BotID: "bot-1"},
		ContextMutations:       contextfrag.NewMutationLedger(),
		ContextBudgetMaxTokens: plan.Window,
		ContextManifest:        contextfrag.Manifest{BudgetPlan: plan},
	}
}

func TestAgentGenerateChecksInitialEnvelopeWithoutReselector(t *testing.T) {
	t.Parallel()

	modelProvider := &atomicMockProvider{handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
		return nil, fmt.Errorf("provider must not be called for an oversized prefix (call %d)", call)
	}}
	a := New(Deps{ContextViewApplier: func(_ context.Context, cfg RunConfig) (RunConfig, error) {
		return cfg, nil
	}})
	plan := contextfrag.ContextBudgetPlan{Window: 2_000, OutputReserve: 500}
	cfg := oversizedPrefixRunConfig(modelProvider, &plan)

	_, err := a.Generate(context.Background(), cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("Generate() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if got := modelProvider.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	records := cfg.ContextMutations.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationContextBudgetFailure {
		t.Fatalf("mutations = %#v, want one context budget failure", records)
	}
}

func TestAgentGenerateChecksFullPrefixInitialEnvelopeWithReselector(t *testing.T) {
	t.Parallel()

	modelProvider := &atomicMockProvider{handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
		return nil, fmt.Errorf("provider must not be called for an oversized prefix (call %d)", call)
	}}
	a := New(Deps{ContextViewApplier: func(_ context.Context, cfg RunConfig) (RunConfig, error) {
		return cfg, nil
	}})
	plan := contextfrag.ContextBudgetPlan{Window: 2_000, OutputReserve: 500}
	cfg := oversizedPrefixRunConfig(modelProvider, &plan)
	reselectorCalls := 0
	cfg.ContextStepReselector = func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult {
		reselectorCalls++
		return ContextStepSelectionResult{}
	}

	_, err := a.Generate(context.Background(), cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("Generate() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if got := modelProvider.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	if reselectorCalls != 0 {
		t.Fatalf("reselector calls = %d, want none for a prefix-only initial dispatch", reselectorCalls)
	}
}

func TestAgentGenerateShadowModeStillFailsClosedOnEnvelopeOverflow(t *testing.T) {
	t.Parallel()

	lookupTool := sdk.Tool{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(_ *sdk.ToolExecContext, _ any) (any, error) {
			return strings.Repeat("large-result ", 1_000), nil
		},
	}
	modelProvider := &atomicMockProvider{handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
		if call != 1 {
			return nil, fmt.Errorf("unexpected provider call %d after envelope overflow", call)
		}
		return &sdk.GenerateResult{
			FinishReason: sdk.FinishReasonToolCalls,
			ToolCalls: []sdk.ToolCall{{
				ToolCallID: "call-shadow", ToolName: "lookup", Input: map[string]any{"q": "one"},
			}},
		}, nil
	}}
	a := New(Deps{
		LoopReselectMode: LoopReselectShadow,
		ContextViewApplier: func(_ context.Context, cfg RunConfig) (RunConfig, error) {
			return cfg, nil
		},
	})
	a.SetToolProviders([]agenttools.ToolProvider{staticToolProvider{tools: []sdk.Tool{lookupTool}}})
	plan := contextfrag.ContextBudgetPlan{Window: 2_000, OutputReserve: 100}
	ledger := contextfrag.NewMutationLedger()

	_, err := a.Generate(context.Background(), RunConfig{
		Model:                  &sdk.Model{ID: "mock-model", Provider: modelProvider},
		System:                 "system",
		Messages:               []sdk.Message{sdk.UserMessage("task")},
		SupportsToolCall:       true,
		Identity:               SessionContext{BotID: "bot-1"},
		ContextMutations:       ledger,
		ContextBudgetMaxTokens: plan.Window,
		ContextManifest:        contextfrag.Manifest{BudgetPlan: &plan},
		ContextStepReselector: func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult {
			return ContextStepSelectionResult{}
		},
	})
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("Generate() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if got := modelProvider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want the initial call only", got)
	}
	if mode := ledger.LoopSelectionMode(); mode != contextfrag.LoopSelectionSuffixOnlyShadow {
		t.Fatalf("loop selection mode = %q, want shadow", mode)
	}
}

func TestStepReselectionAllowanceWithoutPlanReservesResolvedOutput(t *testing.T) {
	t.Parallel()

	cfg := RunConfig{
		Model:                  &sdk.Model{ID: "claude", Provider: anthropicNameMockProvider{&atomicMockProvider{}}},
		ReasoningConfig:        &models.ReasoningConfig{Active: true, Adaptive: true, Effort: models.ReasoningEffortHigh},
		ContextBudgetMaxTokens: 200_000,
		ContextToolDefs:        []contextfrag.ToolDefAccounting{{Name: "lookup", TokenEstimate: 900}},
	}
	if got := stepReselectionAllowance(cfg); got != 200_000-32_000 {
		t.Fatalf("allowance without a plan = %d, want window minus the resolved output reserve", got)
	}
	if got := stepReselectionAllowance(RunConfig{}); got != 0 {
		t.Fatalf("allowance without a window = %d, want budgeting disabled", got)
	}
}
