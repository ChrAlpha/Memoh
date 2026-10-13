package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agentpkg "github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/models"
)

// buildErrorFragsFirstConfig reproduces an internal build error (duplicate
// fragment IDs) so the applier takes the legacy fallback path.
func buildErrorFragsFirstConfig(messages []sdk.Message, window int) agentpkg.RunConfig {
	system := systemTextFrag("system.prompt", "fragment system", contextfrag.KindSystemPrompt, 100)
	first := attentionMessageFrag("duplicate", sdk.UserMessage("first"), 10)
	second := attentionMessageFrag("duplicate", sdk.AssistantMessage("second"), 10)
	return agentpkg.RunConfig{
		System:                 "legacy system",
		Messages:               messages,
		ContextSourceFrags:     []contextfrag.ContextFrag{system, first, second},
		ContextBudgetMaxTokens: window,
	}
}

func TestApplyProviderRunConfigFallbackCarriesBudgetPlanAndStepReselector(t *testing.T) {
	t.Parallel()

	out := ProviderRunConfigApplier(nil)(context.Background(), buildErrorFragsFirstConfig([]sdk.Message{sdk.UserMessage("legacy message")}, 100_000))
	plan := out.ContextManifest.BudgetPlan
	if plan == nil {
		t.Fatal("fallback manifest lost the budget plan")
	}
	limits := models.ResolveGenerationLimits(models.ClientTypeOpenAICompletions, nil, 100_000)
	if plan.Window != 100_000 || plan.OutputReserve != limits.MaxOutputTokens || plan.OutputReserveResolution != limits.Resolution {
		t.Fatalf("fallback plan = %+v, want window 100000 reserving %d (%s)", plan, limits.MaxOutputTokens, limits.Resolution)
	}
	if plan.ActualSystemCost != 0 || plan.HistoryBudget != 0 {
		t.Fatalf("fallback plan = %+v, want the failed build's system and history figures cleared", plan)
	}
	if out.ContextMutations == nil {
		t.Fatal("fallback lost the mutation ledger")
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationContextViewFallback || records[0].Detail != "build_error" {
		t.Fatalf("fallback records = %#v, want one build_error context fallback", records)
	}
}

// A fallback payload over the allowance goes to the provider as it is.
func TestFallbackDispatchSendsLegacyPayloadOverTheAllowance(t *testing.T) {
	t.Parallel()

	provider := &envelopeProbeProvider{handler: func(int, sdk.Request) (sdk.ModelResult, error) {
		return sdk.ModelResult{Text: "ok", FinishReason: sdk.FinishReasonStop}, nil
	}}
	agent := agentpkg.New(agentpkg.Deps{ContextViewApplier: ProviderRunConfigApplier(nil)})
	const window = 2_000
	limits := models.ResolveGenerationLimits(models.ClientTypeOpenAICompletions, nil, window)
	cfg := buildErrorFragsFirstConfig([]sdk.Message{sdk.UserMessage(strings.Repeat("o", 5_100))}, window)
	if cost := contextfrag.ProviderEnvelopeTokens(cfg.System, cfg.Messages, nil); cost <= window-limits.MaxOutputTokens || cost >= window {
		t.Fatalf("legacy payload costs %d tokens, want strictly between allowance %d and window %d", cost, window-limits.MaxOutputTokens, window)
	}
	cfg.Model = &sdk.Model{ID: "model", Provider: provider, Type: sdk.ModelTypeChat}
	cfg.Identity = agentpkg.SessionContext{BotID: "bot-1"}
	cfg.ContextMutations = contextfrag.NewMutationLedger()
	cfg.ContextLifecycle = contextfrag.NewLifecycleHolder()

	if _, err := agent.Generate(context.Background(), cfg); err != nil {
		t.Fatalf("Generate() error = %v, want the payload sent as it is", err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
	records := cfg.ContextMutations.Records()
	if !hasMutation(records, contextfrag.MutationContextViewFallback) || hasMutation(records, contextfrag.MutationContextBudgetFailure) {
		t.Fatalf("mutations = %+v, want the fallback recorded and no budget failure", records)
	}
}
