package contextview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/models"
)

// buildErrorFragsFirstConfig reproduces an internal build error (duplicate
// fragment IDs) so the applier takes the legacy fallback path.
func buildErrorFragsFirstConfig(messages []sdk.Message, window int) agentpkg.RunConfig {
	first := attentionMessageFrag("duplicate", sdk.UserMessage("first"), 10)
	second := attentionMessageFrag("duplicate", sdk.AssistantMessage("second"), 10)
	return agentpkg.RunConfig{
		System:                 "legacy system",
		Messages:               messages,
		ContextSourceFrags:     []contextfrag.ContextFrag{first, second},
		ContextBudgetMaxTokens: window,
	}
}

func TestApplyProviderRunConfigFallbackCarriesBudgetPlanAndStepReselector(t *testing.T) {
	t.Parallel()

	out, err := ProviderRunConfigApplier(nil)(context.Background(), buildErrorFragsFirstConfig([]sdk.Message{sdk.UserMessage("legacy message")}, 100_000))
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want ordinary build fallback", err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil {
		t.Fatal("fallback manifest lost the budget plan")
	}
	limits := models.ResolveGenerationLimits(models.ClientTypeOpenAICompletions, nil, 100_000)
	if plan.Window != 100_000 || plan.OutputReserve != limits.MaxOutputTokens || plan.OutputReserveResolution != limits.Resolution {
		t.Fatalf("fallback plan = %+v, want window 100000 reserving %d (%s)", plan, limits.MaxOutputTokens, limits.Resolution)
	}
	if out.ContextStepReselector == nil {
		t.Fatal("fallback assembly must keep step reselection; it is an assembly path, not a budget-disabled path")
	}
	if out.ContextMutations == nil {
		t.Fatal("fallback lost the mutation ledger")
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationContextViewFallback || records[0].Detail != "build_error" {
		t.Fatalf("fallback records = %#v, want one build_error context fallback", records)
	}
}

func TestFallbackDispatchFailsClosedWhenLegacyPayloadExceedsAllowance(t *testing.T) {
	t.Parallel()

	provider := &envelopeProbeProvider{handler: func(call int, _ sdk.GenerateParams) (*sdk.GenerateResult, error) {
		return nil, fmt.Errorf("provider must not be called for an oversized fallback payload (call %d)", call)
	}}
	agent := agentpkg.New(agentpkg.Deps{ContextViewApplier: ProviderRunConfigApplier(nil)})
	cfg := buildErrorFragsFirstConfig([]sdk.Message{sdk.UserMessage(strings.Repeat("oversized ", 2_000))}, 2_000)
	cfg.Model = &sdk.Model{ID: "model", Provider: provider, Type: sdk.ModelTypeChat}
	cfg.Identity = agentpkg.SessionContext{BotID: "bot-1"}
	cfg.ContextMutations = contextfrag.NewMutationLedger()
	cfg.ContextLifecycle = contextfrag.NewLifecycleHolder()

	_, err := agent.Generate(context.Background(), cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("Generate() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if calls := provider.calls.Load(); calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
	}
	var sawFallback, sawBudgetFailure bool
	for _, record := range cfg.ContextMutations.Records() {
		switch record.Kind {
		case contextfrag.MutationContextViewFallback:
			sawFallback = true
		case contextfrag.MutationContextBudgetFailure:
			sawBudgetFailure = true
		}
	}
	if !sawFallback || !sawBudgetFailure {
		t.Fatalf("mutations = %+v, want both the fallback and the budget failure recorded", cfg.ContextMutations.Records())
	}
}
