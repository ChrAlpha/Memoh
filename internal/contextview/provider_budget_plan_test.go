package contextview

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func TestProviderContextBudgetPlanAccountsForCurrentRequestAndTools(t *testing.T) {
	t.Parallel()

	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "current", Kind: contextfrag.KindCurrentUserMessage, Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotCurrentUser, Text: "current request", Trust: contextfrag.TrustUser,
	})
	current.TokenEstimate = 120
	image := contextfrag.ImageFrag("image", []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}}, contextfrag.Scope{}, contextfrag.SourceRunConfig)
	history := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID: "history", Message: sdk.UserMessage("old"), Kind: contextfrag.KindConversationEvent,
		Slot: contextfrag.SlotHistory, TokenEstimate: 900,
	})
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: 20000,
		ContextSourceFrags:     []contextfrag.ContextFrag{history, current, image},
		ContextToolDefs: []contextfrag.ToolDefAccounting{
			{Name: "one", TokenEstimate: 100},
			{Name: "two", TokenEstimate: 200},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.Window != 20000 || plan.OutputReserve != 5000 || plan.ToolDefsCost != 300 ||
		plan.CurrentRequestCost != 120+contextfrag.EstimateImageTokens {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Estimator != contextfrag.ProviderBudgetEstimator ||
		plan.EstimatorSafetyFactorPercent != contextfrag.ProviderBudgetSafetyFactorPercent {
		t.Fatalf("estimator contract = %q/%d", plan.Estimator, plan.EstimatorSafetyFactorPercent)
	}
}

func TestProviderContextBudgetPlanUsesConservativeByteCosts(t *testing.T) {
	t.Parallel()

	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "current", Kind: contextfrag.KindCurrentUserMessage, Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotCurrentUser, Text: "abcdefghijklmnop",
	})
	current.TokenEstimate = 1
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: 10000,
		ContextSourceFrags:     []contextfrag.ContextFrag{current},
		ContextToolDefs:        []contextfrag.ToolDefAccounting{{Name: "short", Bytes: 16, TokenEstimate: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ToolDefsCost != 5 || plan.CurrentRequestCost != 5 {
		t.Fatalf("provider costs = tools %d current %d, want 5/5", plan.ToolDefsCost, plan.CurrentRequestCost)
	}
}

func TestProviderContextBudgetPlanCountsSemanticCurrentInHistorySlot(t *testing.T) {
	t.Parallel()

	current := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID: "discuss.current", Message: sdk.UserMessage("current"), Kind: contextfrag.KindCurrentUserMessage,
		Slot: contextfrag.SlotHistory, TokenEstimate: 123, Budget: contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: 10000,
		ContextSourceFrags:     []contextfrag.ContextFrag{current},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.CurrentRequestCost != 123 {
		t.Fatalf("CurrentRequestCost = %d, want 123", plan.CurrentRequestCost)
	}
}

func TestProviderContextBudgetPlanOutputReserveCrossover(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		window int
		want   int
	}{{32767, 8191}, {32768, 8192}, {32769, 8192}} {
		plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{ContextBudgetMaxTokens: tt.window})
		if err != nil {
			t.Fatalf("window %d: %v", tt.window, err)
		}
		if plan == nil || plan.OutputReserve != tt.want {
			t.Fatalf("window %d plan = %#v, want reserve %d", tt.window, plan, tt.want)
		}
	}
}

func TestProviderContextBudgetPlanRejectsImpossibleWindow(t *testing.T) {
	t.Parallel()

	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "current", Kind: contextfrag.KindCurrentUserMessage, Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotCurrentUser, Text: "oversized", Trust: contextfrag.TrustUser,
	})
	current.TokenEstimate = 6000
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: 8192,
		ContextSourceFrags:     []contextfrag.ContextFrag{current},
		ContextToolDefs:        []contextfrag.ToolDefAccounting{{Name: "tool", TokenEstimate: 100}},
	})
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("error = %v, want ErrBudgetUnsatisfied", err)
	}
	if plan == nil || plan.OutputReserve != 2048 || plan.SystemBudget != MinimumSystemBudgetTokens {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestProviderContextBudgetPlanUsesMaterializedRequestEstimate(t *testing.T) {
	t.Parallel()

	current := 1
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		Messages: []sdk.Message{sdk.AssistantMessage("old"), sdk.UserMessage("materialized")},
		Query:    "do not count twice", InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,ignored"}},
		ContextQueryMaterialized: true, ContextCurrentUserMessageIndex: &current,
		ContextHistoryTokenEstimates: []int{77, 222}, ContextBudgetMaxTokens: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.CurrentRequestCost != 222 {
		t.Fatalf("CurrentRequestCost = %d, want 222", plan.CurrentRequestCost)
	}
}

func TestProviderContextBudgetPlanDisabledWithoutWindow(t *testing.T) {
	t.Parallel()

	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{})
	if err != nil || plan != nil {
		t.Fatalf("plan/error = %#v/%v, want nil/nil", plan, err)
	}
}

func TestProviderContextBudgetPlanRejectsNegativeWindow(t *testing.T) {
	t.Parallel()

	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{ContextBudgetMaxTokens: -1})
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) || plan == nil {
		t.Fatalf("plan/error = %#v/%v, want plan and ErrBudgetUnsatisfied", plan, err)
	}
}

func TestResolveRecentProtectTokens(t *testing.T) {
	t.Parallel()

	zero := 0
	negative := -1
	if got := resolveRecentProtectTokens(nil); got != DefaultRecentProtectTokens {
		t.Fatalf("default = %d", got)
	}
	if got := resolveRecentProtectTokens(&zero); got != 0 {
		t.Fatalf("zero override = %d", got)
	}
	if got := resolveRecentProtectTokens(&negative); got != 0 {
		t.Fatalf("negative override = %d", got)
	}
}

func TestProviderBudgetPlanIsAppliedAndRetained(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "current", Kind: contextfrag.KindCurrentUserMessage, Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotCurrentUser, Text: "current", Trust: contextfrag.TrustUser,
	})
	current.TokenEstimate = 120
	out, err := applyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
		ContextSourceFrags: []contextfrag.ContextFrag{current}, ContextBudgetMaxTokens: 8192,
		ContextLifecycle: holder,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil || plan.OutputReserve != 2048 || plan.CurrentRequestCost != 120 || plan.SystemBudget != 6024 {
		t.Fatalf("budget plan = %#v", plan)
	}
	snapshot, ok := holder.Snapshot()
	if !ok || snapshot.BudgetPlan == nil || snapshot.BudgetPlan.SystemBudget != 6024 {
		t.Fatalf("holder snapshot = %#v, %v", snapshot, ok)
	}
}

func TestProviderBudgetFailureBypassesLegacyFallbackAndKeepsAudit(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	required := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "system.required", Kind: contextfrag.KindSystemPrompt, Role: sdk.MessageRoleSystem,
		Slot: contextfrag.SlotSystem, Text: "required", Trust: contextfrag.TrustSystem,
		RetentionTier: contextfrag.RetentionRequired,
	})
	required.TokenEstimate = 930
	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "current", Kind: contextfrag.KindCurrentUserMessage, Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotCurrentUser, Text: "current", Trust: contextfrag.TrustUser,
	})
	current.TokenEstimate = 10
	out, err := applyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
		System: "legacy must not be selected", Messages: []sdk.Message{sdk.UserMessage("legacy")},
		ContextSourceFrags: []contextfrag.ContextFrag{required, current}, ContextBudgetMaxTokens: 400,
		ContextLifecycle: holder,
	})
	if !errors.Is(err, contextfrag.ErrProtectedContextOverflow) {
		t.Fatalf("error = %v, want ErrProtectedContextOverflow", err)
	}
	if out.ContextManifest.BudgetPlan == nil || out.ContextManifest.Selection == nil {
		t.Fatalf("failure manifest = %#v", out.ContextManifest)
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationContextBudgetFailure {
		t.Fatalf("mutations = %#v", records)
	}
	for _, record := range records {
		if record.Kind == contextfrag.MutationContextViewFallback {
			t.Fatalf("budget failure used legacy fallback: %#v", records)
		}
	}
	snapshot, ok := holder.Snapshot()
	if !ok || snapshot.BudgetPlan == nil || len(snapshot.Mutations) != 1 {
		t.Fatalf("failure snapshot = %#v, %v", snapshot, ok)
	}
}

func TestMissingContextWindowIsAuditedWithoutChangingProviderBytes(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{
		System: "legacy system", Messages: []sdk.Message{sdk.AssistantMessage("legacy")}, Query: "current",
		CurrentModelID: "model-1", ContextScope: contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"},
		ContextLifecycle: holder,
	}
	out, err := applyProviderRunConfig(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.System != cfg.System || !reflect.DeepEqual(out.Messages, []sdk.Message{sdk.AssistantMessage("legacy"), sdk.UserMessage("current")}) {
		t.Fatalf("provider bytes changed: system=%q messages=%#v", out.System, out.Messages)
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 || records[0] != (contextfrag.MutationRecord{Kind: contextfrag.MutationContextBudgetDisabled, Detail: "missing_context_window"}) {
		t.Fatalf("mutations = %#v", records)
	}
	if out.ContextManifest.BudgetPlan != nil {
		t.Fatalf("disabled plan = %#v", out.ContextManifest.BudgetPlan)
	}
	for _, want := range []string{"missing_context_window", "bot-1", "session-1", "model-1"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("warning %q missing %q", logs.String(), want)
		}
	}
	snapshot, ok := holder.Snapshot()
	if !ok || len(snapshot.Mutations) != 1 || snapshot.Mutations[0].Kind != contextfrag.MutationContextBudgetDisabled {
		t.Fatalf("disabled snapshot = %#v, %v", snapshot, ok)
	}
}
