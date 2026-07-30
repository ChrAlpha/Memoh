package contextview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

func contextWindowForDefaultOutputReserve(inputBudget int) int {
	if inputBudget <= 0 {
		return 0
	}
	window := inputBudget
	for {
		resolved := inputBudget + min(DefaultOutputReserveTokens, window/4)
		if resolved == window {
			return window
		}
		window = resolved
	}
}

func activeHistoryInputBudget(legacyBudget int) (inputBudget, scale int) {
	if legacyBudget <= 0 {
		return 0, 1
	}
	scale = 2
	if scaled := legacyBudget * scale; scaled < MinimumSystemBudgetTokens {
		scale = (MinimumSystemBudgetTokens + legacyBudget - 1) / legacyBudget
	}
	noticeCost := contextfrag.ResolveFragTokens(TrimNoticeFrag(contextfrag.Scope{}))
	return legacyBudget*scale + noticeCost, scale
}

func activateHistoryBudget(cfg *agentpkg.RunConfig, legacyBudget int) {
	inputBudget, scale := activeHistoryInputBudget(legacyBudget)
	for i := range cfg.ContextHistoryTokenEstimates {
		cfg.ContextHistoryTokenEstimates[i] *= scale
	}
	for i := range cfg.ContextSourceFrags {
		cfg.ContextSourceFrags[i].TokenEstimate *= scale
	}
	frags := cfg.ContextSourceFrags
	if len(frags) == 0 {
		frags = CollectNonSystemProviderSourceFrags(context.Background(), *cfg)
	}
	tagged := tagFragments(frags, (&FragmentSelector{}).ProfileFor(contextfrag.IntentRunConfigPreProvider))
	inputBudget += protectedHistoryTokenCost(tagged)
	currentRequestCost, err := providerCurrentRequestCost(context.Background(), *cfg)
	if err != nil {
		panic(err)
	}
	toolDefsCost := 0
	for _, def := range cfg.ContextToolDefs {
		toolDefsCost += def.TokenEstimate
	}
	inputBudget += currentRequestCost + toolDefsCost
	cfg.ContextBudgetMaxTokens = contextWindowForDefaultOutputReserve(inputBudget)
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
	const wantReserve = 5000
	if plan.Window != 20000 ||
		plan.OutputReserve != wantReserve ||
		plan.ToolDefsCost != 300 ||
		plan.CurrentRequestCost != wantCurrent {
		t.Fatalf("plan = %#v, want window/output/tools/current = 20000/%d/300/%d",
			plan, wantReserve, wantCurrent)
	}
	wantSystem := 20000 - wantReserve - 300 - wantCurrent
	if plan.SystemBudget != wantSystem {
		t.Fatalf("SystemBudget = %d, want %d", plan.SystemBudget, wantSystem)
	}
	if plan.Estimator != contextfrag.ProviderBudgetEstimator ||
		plan.EstimatorSafetyFactorPercent != contextfrag.ProviderBudgetSafetyFactorPercent {
		t.Fatalf("estimator contract = %q/%d, want %q/%d",
			plan.Estimator,
			plan.EstimatorSafetyFactorPercent,
			contextfrag.ProviderBudgetEstimator,
			contextfrag.ProviderBudgetSafetyFactorPercent,
		)
	}
}

func TestProviderContextBudgetPlanUsesConservativeByteCosts(t *testing.T) {
	t.Parallel()

	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:   "current",
		Kind: contextfrag.KindCurrentUserMessage,
		Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotCurrentUser,
		Text: "abcdefghijklmnop",
	})
	current.TokenEstimate = 1
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: 10000,
		ContextSourceFrags:     []contextfrag.ContextFrag{current},
		ContextToolDefs: []contextfrag.ToolDefAccounting{{
			Name:          "short_schema",
			Bytes:         16,
			TokenEstimate: 1,
		}},
	})
	if err != nil {
		t.Fatalf("providerContextBudgetPlan() error = %v", err)
	}
	if plan.ToolDefsCost != 5 || plan.CurrentRequestCost != 5 {
		t.Fatalf("provider costs = tools %d current %d, want conservative 5/5",
			plan.ToolDefsCost, plan.CurrentRequestCost)
	}
}

func TestApplyProviderRunConfigSmallWindowUsesScaledOutputReserve(t *testing.T) {
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
	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{
		ContextSourceFrags: []contextfrag.ContextFrag{history, current},
		ContextToolDefs: []contextfrag.ToolDefAccounting{
			{Name: "tool", TokenEstimate: 100},
		},
		ContextBudgetMaxTokens: 8192,
		ContextLifecycle:       holder,
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want small-window turn to proceed", err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil {
		t.Fatal("small-window turn lost its active budget plan")
	}
	if plan.OutputReserve != 2048 ||
		plan.ToolDefsCost != 100 ||
		plan.CurrentRequestCost != 120 ||
		plan.SystemBudget != 5924 {
		t.Fatalf("budget plan = %#v, want reserve/tools/current/system = 2048/100/120/5924", plan)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages = %#v, want history and current request", out.Messages)
	}
	snapshot, ok := holder.Snapshot()
	if !ok ||
		snapshot.BudgetPlan == nil ||
		snapshot.BudgetPlan.OutputReserve != 2048 ||
		snapshot.BudgetPlan.Estimator != contextfrag.ProviderBudgetEstimator ||
		snapshot.BudgetPlan.EstimatorSafetyFactorPercent != contextfrag.ProviderBudgetSafetyFactorPercent {
		t.Fatalf("lifecycle snapshot = %#v, %v; want reserve and estimator contract", snapshot, ok)
	}
}

func TestApplyProviderRunConfigSemanticCurrentIsNotDoubleCharged(t *testing.T) {
	t.Parallel()

	requiredSystem := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:            "system.required",
		Kind:          contextfrag.KindSystemPrompt,
		Role:          sdk.MessageRoleSystem,
		Slot:          contextfrag.SlotSystem,
		Text:          "required system",
		RetentionTier: contextfrag.RetentionRequired,
		Trust:         contextfrag.TrustSystem,
	})
	requiredSystem.TokenEstimate = 200
	semanticCurrent := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:            "discuss.current",
		Message:       sdk.UserMessage("current discuss request"),
		Kind:          contextfrag.KindCurrentUserMessage,
		Slot:          contextfrag.SlotHistory,
		TokenEstimate: 100,
		Trust:         contextfrag.TrustUser,
		Budget:        contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
	cfg := agentpkg.RunConfig{
		ContextSourceFrags:     []contextfrag.ContextFrag{requiredSystem, semanticCurrent},
		ContextBudgetMaxTokens: contextWindowForDefaultOutputReserve(356),
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want payload that fits its provider envelope", err)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil ||
		plan.CurrentRequestCost != 100 ||
		plan.SystemBudget != 256 ||
		plan.ActualSystemCost != 200 ||
		plan.HistoryBudget != 56 {
		t.Fatalf("budget plan = %#v, want current/system/actual/history = 100/256/200/56", plan)
	}
	if len(out.Messages) != 1 || !sdkMessagesJSONEqual(out.Messages[0], sdk.UserMessage("current discuss request")) {
		t.Fatalf("messages = %#v, want the semantic current request exactly once", out.Messages)
	}
}

func TestProviderContextBudgetPlanDefaultOutputReserveCrossover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		window      int
		wantReserve int
	}{
		{name: "below", window: 32767, wantReserve: 8191},
		{name: "at", window: 32768, wantReserve: DefaultOutputReserveTokens},
		{name: "above", window: 32769, wantReserve: DefaultOutputReserveTokens},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
				ContextBudgetMaxTokens: tt.window,
			})
			if err != nil {
				t.Fatalf("providerContextBudgetPlan() error = %v", err)
			}
			if plan == nil ||
				plan.OutputReserve != tt.wantReserve ||
				plan.SystemBudget != tt.window-tt.wantReserve {
				t.Fatalf("plan = %#v, want reserve/system = %d/%d",
					plan, tt.wantReserve, tt.window-tt.wantReserve)
			}
		})
	}
}

func TestProviderContextBudgetPlanRejectsGenuinelyImpossibleSmallWindow(t *testing.T) {
	t.Parallel()

	current := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:    "current",
		Kind:  contextfrag.KindCurrentUserMessage,
		Role:  sdk.MessageRoleUser,
		Slot:  contextfrag.SlotCurrentUser,
		Text:  "oversized current request",
		Trust: contextfrag.TrustUser,
	})
	current.TokenEstimate = 6000
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: 8192,
		ContextSourceFrags:     []contextfrag.ContextFrag{current},
		ContextToolDefs: []contextfrag.ToolDefAccounting{
			{Name: "tool", TokenEstimate: 100},
		},
	})

	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("providerContextBudgetPlan() error = %v, want ErrBudgetUnsatisfied", err)
	}
	if plan == nil ||
		plan.OutputReserve != 2048 ||
		plan.ToolDefsCost != 100 ||
		plan.CurrentRequestCost != 6000 ||
		plan.SystemBudget != MinimumSystemBudgetTokens {
		t.Fatalf("plan = %#v, want resolved reserve and unchanged actual costs", plan)
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

func TestProviderContextBudgetPlanDisabledWithoutWindow(t *testing.T) {
	t.Parallel()

	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{})
	if err != nil {
		t.Fatalf("providerContextBudgetPlan() error = %v", err)
	}
	if plan != nil {
		t.Fatalf("providerContextBudgetPlan() = %#v, want nil", plan)
	}
}

func TestApplyProviderRunConfigAuditsMissingContextWindow(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{
		System:         "legacy system",
		Messages:       []sdk.Message{sdk.AssistantMessage("legacy")},
		Query:          "current",
		CurrentModelID: "model-1",
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
		ContextLifecycle: holder,
	}

	out, err := ApplyProviderRunConfig(context.Background(), logger, cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v", err)
	}
	records := out.ContextMutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationContextBudgetDisabled ||
		records[0].Detail != "missing_context_window" {
		t.Fatalf("mutations = %#v, want exactly one missing-window audit", records)
	}
	if out.ContextManifest.BudgetPlan != nil {
		t.Fatalf("budget plan = %#v, want disabled", out.ContextManifest.BudgetPlan)
	}
	wantMessages := []sdk.Message{sdk.AssistantMessage("legacy"), sdk.UserMessage("current")}
	if out.System != cfg.System || !reflect.DeepEqual(out.Messages, wantMessages) {
		t.Fatalf("provider payload = system %q messages %#v, want legacy unbudgeted result", out.System, out.Messages)
	}
	snapshot, ok := holder.Snapshot()
	if !ok || len(snapshot.Mutations) != 1 ||
		snapshot.Mutations[0].Kind != contextfrag.MutationContextBudgetDisabled ||
		snapshot.Mutations[0].Detail != "missing_context_window" {
		t.Fatalf("lifecycle snapshot = %#v, %v; want missing-window audit", snapshot, ok)
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("warning logs = %q, want exactly one record", logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("warning log is not structured JSON: %v", err)
	}
	if record["level"] != "WARN" ||
		record["bot_id"] != "bot-1" ||
		record["session_id"] != "session-1" ||
		record["model_id"] != "model-1" {
		t.Fatalf("warning log = %#v, want WARN with bot/session/model attrs", record)
	}
}

func TestProviderContextBudgetPlanEnabledForDiscuss(t *testing.T) {
	t.Parallel()

	const window = 10000
	plan, err := providerContextBudgetPlan(context.Background(), agentpkg.RunConfig{
		ContextBudgetMaxTokens: window,
		SessionType:            sessionmode.Discuss,
	})
	if err != nil {
		t.Fatalf("providerContextBudgetPlan() error = %v", err)
	}
	if plan == nil || plan.Window != window {
		t.Fatalf("providerContextBudgetPlan() = %#v, want active discuss plan with window %d", plan, window)
	}
}

func TestApplyProviderRunConfigRejectsNegativeWindowWithPlan(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{
		System:                 "required system",
		ContextBudgetMaxTokens: -1,
		ContextLifecycle:       holder,
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil || plan.Window != -1 {
		t.Fatalf("budget plan = %#v, want active plan with window -1", plan)
	}
	records := out.ContextManifest.Mutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationContextBudgetFailure ||
		records[0].Detail != "budget_unsatisfied" {
		t.Fatalf("budget failure mutations = %#v, want one stable failure record", records)
	}
	snapshot, ok := holder.Snapshot()
	if !ok || snapshot.BudgetPlan == nil || snapshot.BudgetPlan.Window != -1 {
		t.Fatalf("lifecycle snapshot = %#v, %v; want negative-window plan", snapshot, ok)
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
		ContextBudgetMaxTokens: contextWindowForDefaultOutputReserve(80 + 120 + 500),
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

func TestApplyProviderRunConfigBudgetErrorKeepsAuditWhenRenderFails(t *testing.T) {
	t.Parallel()

	first := attentionMessageFrag("duplicate", sdk.UserMessage("first"), 10)
	second := attentionMessageFrag("duplicate", sdk.AssistantMessage("second"), 10)
	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{
		System:                 "legacy system",
		Messages:               []sdk.Message{sdk.UserMessage("legacy message")},
		ContextSourceFrags:     []contextfrag.ContextFrag{first, second},
		ContextBudgetMaxTokens: 100,
		ContextLifecycle:       holder,
	}

	out, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("ApplyProviderRunConfig() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if out.System != cfg.System || !reflect.DeepEqual(out.Messages, cfg.Messages) {
		t.Fatalf("budget failure changed provider payload: system=%q messages=%#v", out.System, out.Messages)
	}
	plan := out.ContextManifest.BudgetPlan
	if plan == nil || plan.Window != 100 {
		t.Fatalf("budget plan = %#v, want retained 100-token window", plan)
	}
	records := out.ContextManifest.Mutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationContextBudgetFailure ||
		records[0].Detail != "budget_unsatisfied" {
		t.Fatalf("budget failure mutations = %#v, want one stable failure record", records)
	}
	if out.ContextMutations == nil {
		t.Fatal("budget failure lost the run-config mutation ledger")
	}
	snapshot, ok := holder.Snapshot()
	if !ok || snapshot.BudgetPlan == nil || snapshot.BudgetPlan.Window != 100 {
		t.Fatalf("lifecycle snapshot = %#v, %v; want retained plan", snapshot, ok)
	}
	if len(snapshot.Mutations) != 1 ||
		snapshot.Mutations[0].Kind != contextfrag.MutationContextBudgetFailure {
		t.Fatalf("lifecycle mutations = %#v, want budget failure", snapshot.Mutations)
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
		ContextBudgetMaxTokens: contextWindowForDefaultOutputReserve(300),
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
		System:                 "legacy system",
		Messages:               []sdk.Message{sdk.UserMessage("legacy message")},
		ContextSourceFrags:     []contextfrag.ContextFrag{first, second},
		ContextBudgetMaxTokens: 100000,
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
