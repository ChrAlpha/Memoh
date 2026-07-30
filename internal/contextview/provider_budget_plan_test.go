package contextview

import (
	"context"
	"errors"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

func activeHistoryWindow(legacyBudget int) (window, scale int) {
	if legacyBudget <= 0 {
		return 0, 1
	}
	scale = 2
	if scaled := legacyBudget * scale; scaled < MinimumSystemBudgetTokens {
		scale = (MinimumSystemBudgetTokens + legacyBudget - 1) / legacyBudget
	}
	noticeCost := contextfrag.ResolveFragTokens(TrimNoticeFrag(contextfrag.Scope{}))
	return DefaultOutputReserveTokens + legacyBudget*scale + noticeCost, scale
}

func activateHistoryBudget(cfg *agentpkg.RunConfig, legacyBudget int) {
	window, scale := activeHistoryWindow(legacyBudget)
	for i := range cfg.ContextHistoryTokenEstimates {
		cfg.ContextHistoryTokenEstimates[i] *= scale
	}
	for i := range cfg.ContextSourceFrags {
		cfg.ContextSourceFrags[i].TokenEstimate *= scale
	}
	currentRequestCost, err := providerCurrentRequestCost(context.Background(), *cfg)
	if err != nil {
		panic(err)
	}
	toolDefsCost := 0
	for _, def := range cfg.ContextToolDefs {
		toolDefsCost += def.TokenEstimate
	}
	cfg.ContextBudgetMaxTokens = window + currentRequestCost + toolDefsCost
}

func TestProviderContextBudgetPlanAccountsForSourceCurrentRequestAndTools(t *testing.T) {
	t.Parallel()

	currentText := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:    "current.text",
		Kind:  contextfrag.KindCurrentUserMessage,
		Role:  sdk.MessageRoleUser,
		Slot:  contextfrag.SlotCurrentUser,
		Text:  "current request",
		Trust: contextfrag.TrustUser,
	})
	currentText.TokenEstimate = 120
	currentImage := contextfrag.ImageFrag(
		"current.image",
		[]sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
		contextfrag.Scope{},
		contextfrag.SourceRunConfig,
	)
	history := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:            "history",
		Message:       sdk.UserMessage("old"),
		Kind:          contextfrag.KindConversationEvent,
		Slot:          contextfrag.SlotHistory,
		TokenEstimate: 900,
	})
	cfg := agentpkg.RunConfig{
		ContextBudgetMaxTokens: 20000,
		ContextSourceFrags:     []contextfrag.ContextFrag{history, currentText, currentImage},
		ContextToolDefs: []contextfrag.ToolDefAccounting{
			{Name: "one", TokenEstimate: 100},
			{Name: "two", TokenEstimate: 200},
		},
	}

	plan, err := providerContextBudgetPlan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("providerContextBudgetPlan() error = %v", err)
	}
	if plan == nil {
		t.Fatal("providerContextBudgetPlan() = nil, want active plan")
	}
	wantCurrent := 120 + contextfrag.EstimateImageTokens
	if plan.Window != 20000 ||
		plan.OutputReserve != DefaultOutputReserveTokens ||
		plan.ToolDefsCost != 300 ||
		plan.CurrentRequestCost != wantCurrent {
		t.Fatalf("plan = %#v, want window/output/tools/current = 20000/%d/300/%d",
			plan, DefaultOutputReserveTokens, wantCurrent)
	}
	wantSystem := 20000 - DefaultOutputReserveTokens - 300 - wantCurrent
	if plan.SystemBudget != wantSystem {
		t.Fatalf("SystemBudget = %d, want %d", plan.SystemBudget, wantSystem)
	}
}

func TestProviderContextBudgetPlanUsesLegacyMaterializedRequestSemantics(t *testing.T) {
	t.Parallel()

	currentIndex := 1
	cfg := agentpkg.RunConfig{
		Messages: []sdk.Message{
			sdk.AssistantMessage("old"),
			sdk.UserMessage("already materialized"),
		},
		Query:                          "must not be counted again",
		InlineImages:                   []sdk.ImagePart{{Image: "data:image/png;base64,ignored", MediaType: "image/png"}},
		ContextQueryMaterialized:       true,
		ContextCurrentUserMessageIndex: &currentIndex,
		ContextHistoryTokenEstimates:   []int{77, 222},
		ContextBudgetMaxTokens:         10000,
	}

	plan, err := providerContextBudgetPlan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("providerContextBudgetPlan() error = %v", err)
	}
	if plan.CurrentRequestCost != 222 {
		t.Fatalf("CurrentRequestCost = %d, want only materialized estimate 222", plan.CurrentRequestCost)
	}
}

func TestProviderContextBudgetPlanDisabledWithoutWindowOrForDiscuss(t *testing.T) {
	t.Parallel()

	for _, cfg := range []agentpkg.RunConfig{
		{},
		{ContextBudgetMaxTokens: 10000, SessionType: sessionmode.Discuss},
	} {
		plan, err := providerContextBudgetPlan(context.Background(), cfg)
		if err != nil {
			t.Fatalf("providerContextBudgetPlan(%#v) error = %v", cfg, err)
		}
		if plan != nil {
			t.Fatalf("providerContextBudgetPlan(%#v) = %#v, want nil", cfg, plan)
		}
	}
}

func TestApplyProviderRunConfigStoresActivePlanWithoutNoPressureMutation(t *testing.T) {
	t.Parallel()

	history := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:            "history",
		Message:       sdk.UserMessage("old context"),
		Kind:          contextfrag.KindConversationEvent,
		Slot:          contextfrag.SlotHistory,
		TokenEstimate: 100,
	})
	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:    "current",
		Kind:  contextfrag.KindCurrentUserMessage,
		Role:  sdk.MessageRoleUser,
		Slot:  contextfrag.SlotCurrentUser,
		Text:  "current request",
		Trust: contextfrag.TrustUser,
	})
	current.TokenEstimate = 120
	cfg := agentpkg.RunConfig{
		ContextSourceFrags: []contextfrag.ContextFrag{history, current},
		ContextToolDefs: []contextfrag.ToolDefAccounting{
			{Name: "tool", TokenEstimate: 80},
		},
		ContextBudgetMaxTokens: DefaultOutputReserveTokens + 80 + 120 + 500,
	}

	disabled := cfg
	disabled.ContextBudgetMaxTokens = 0
	legacyOut, err := ApplyProviderRunConfig(context.Background(), nil, disabled)
	if err != nil {
		t.Fatalf("plan-disabled ApplyProviderRunConfig() error = %v", err)
	}
	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v", err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil {
		t.Fatal("successful active run lost its budget plan")
	}
	if plan.SystemBudget != 500 || plan.ActualSystemCost != 0 || plan.HistoryBudget != 500 {
		t.Fatalf("budget plan = %#v, want system/actual/history = 500/0/500", plan)
	}
	if out.ContextMutations == nil || len(out.ContextMutations.Records()) != 0 {
		t.Fatalf("no-pressure mutations = %#v, want zero", out.ContextMutations)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %#v, want history and current request", out.Messages)
	}
	if out.System != legacyOut.System ||
		out.Query != legacyOut.Query ||
		!reflect.DeepEqual(out.Messages, legacyOut.Messages) ||
		!reflect.DeepEqual(out.InlineImages, legacyOut.InlineImages) {
		t.Fatalf("active no-pressure payload diverged:\nactive=%#v\nlegacy=%#v", out, legacyOut)
	}
}

func TestApplyProviderRunConfigWithBudgetErrorKeepsAuditWithoutFallback(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{
		System:                 "required system",
		Query:                  "current request",
		ContextBudgetMaxTokens: 100,
		ContextLifecycle:       holder,
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if out.ContextManifest.BudgetPlan == nil {
		t.Fatal("budget failure lost the numeric plan")
	}
	records := out.ContextManifest.Mutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationContextBudgetFailure ||
		records[0].Detail != "budget_unsatisfied" {
		t.Fatalf("budget failure mutations = %#v, want one stable failure record", records)
	}
	if out.System != cfg.System || len(out.Messages) != 0 {
		t.Fatalf("budget failure changed provider payload: system=%q messages=%#v", out.System, out.Messages)
	}
	snapshot, ok := holder.Snapshot()
	if !ok || snapshot.BudgetPlan == nil {
		t.Fatalf("lifecycle snapshot = %#v, %v; want budget plan", snapshot, ok)
	}
}

func TestApplyProviderRunConfigProtectedOverflowKeepsSelectionAudit(t *testing.T) {
	t.Parallel()

	required := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:            "system.required",
		Kind:          contextfrag.KindSystemPrompt,
		Role:          sdk.MessageRoleSystem,
		Slot:          contextfrag.SlotSystem,
		Text:          "required",
		RetentionTier: contextfrag.RetentionRequired,
		Trust:         contextfrag.TrustSystem,
	})
	required.TokenEstimate = 1000
	cfg := agentpkg.RunConfig{
		ContextSourceFrags:     []contextfrag.ContextFrag{required},
		ContextBudgetMaxTokens: DefaultOutputReserveTokens + 300,
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if !errors.Is(err, contextfrag.ErrProtectedContextOverflow) {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want %v", err, contextfrag.ErrProtectedContextOverflow)
	}
	if out.ContextManifest.BudgetPlan == nil || out.ContextManifest.BudgetPlan.ActualSystemCost != 1000 {
		t.Fatalf("budget plan = %#v, want audited actual system cost 1000", out.ContextManifest.BudgetPlan)
	}
	if len(out.ContextManifest.SelectionDecisions) == 0 {
		t.Fatal("protected overflow lost selection decisions")
	}
	records := out.ContextManifest.Mutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationContextBudgetFailure ||
		records[0].Detail != "protected_context_overflow" {
		t.Fatalf("protected overflow records = %#v, want one stable failure record", records)
	}
}

func TestProviderSelectorReservesHistoryTrimNoticeWithinPlan(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("old-1", sdk.UserMessage("old one"), 100),
		attentionMessageFrag("old-2", sdk.AssistantMessage("old two"), 100),
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:    "current",
			Kind:  contextfrag.KindCurrentUserMessage,
			Role:  sdk.MessageRoleUser,
			Slot:  contextfrag.SlotCurrentUser,
			Text:  "current",
			Trust: contextfrag.TrustUser,
		}),
	}
	plan := &contextfrag.ContextBudgetPlan{SystemBudget: 120}

	result := (&FragmentSelector{}).Select(
		frags,
		(&FragmentSelector{}).ProfileFor(contextfrag.IntentRunConfigPreProvider),
		BudgetEnvelope{Plan: plan},
	)

	if result.FatalError != nil {
		t.Fatalf("Select() error = %v", result.FatalError)
	}
	if !result.TrimNotice {
		t.Fatal("history pressure must retain its notice")
	}
	if len(result.Dropped) != 2 {
		t.Fatalf("dropped = %#v, want both 100-token history fragments so the notice fits", result.Dropped)
	}
	noticeCost := contextfrag.ResolveFragTokens(TrimNoticeFrag(contextfrag.Scope{}))
	if noticeCost > plan.HistoryBudget {
		t.Fatalf("notice cost %d exceeds history budget %d", noticeCost, plan.HistoryBudget)
	}
}

func TestApplyProviderRunConfigInternalBuildErrorStillFallsBack(t *testing.T) {
	t.Parallel()

	first := attentionMessageFrag("duplicate", sdk.UserMessage("first"), 10)
	second := attentionMessageFrag("duplicate", sdk.AssistantMessage("second"), 10)
	cfg := agentpkg.RunConfig{
		System:             "legacy system",
		Messages:           []sdk.Message{sdk.UserMessage("legacy message")},
		ContextSourceFrags: []contextfrag.ContextFrag{first, second},
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want ordinary build fallback", err)
	}
	if out.System != cfg.System || !reflect.DeepEqual(out.Messages, cfg.Messages) {
		t.Fatalf("fallback payload = system %q messages %#v, want legacy payload", out.System, out.Messages)
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationContextViewFallback ||
		records[0].Detail != "build_error" {
		t.Fatalf("fallback records = %#v, want one build_error context fallback", records)
	}
}
