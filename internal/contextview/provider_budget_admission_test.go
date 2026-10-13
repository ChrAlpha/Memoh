package contextview

import (
	"errors"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	native "github.com/felinics/memoh/internal/agent/runtime/native"
)

func providerBudgetFixture() native.RunConfig {
	history := stableHistoryMessageFrag("history", sdk.AssistantMessage(strings.Repeat("h", 8000)))
	history.Budget.Overflow = contextfrag.OverflowKeep
	return native.RunConfig{
		ContextBudgetMaxTokens:  16000,
		ContextToolDefsResolved: true,
		ContextToolDefs:         []contextfrag.ToolDefAccounting{{Name: "large_roster", TokenEstimate: 10000}},
		ContextSourceFrags: []contextfrag.ContextFrag{
			systemTextFrag("system", strings.Repeat("s", 3200), contextfrag.KindSystemPrompt, 20),
			history,
			currentMessageFrag("current", "continue"),
		},
		ContextLifecycle: contextfrag.NewLifecycleHolder(),
	}
}

func TestProviderBudgetTrimsDroppableHistoryInOnePass(t *testing.T) {
	cfg := providerBudgetFixture()
	cfg.ContextSourceFrags[1].Budget.Overflow = contextfrag.OverflowDrop
	got, err := ProviderRunConfigApplier(nil)(t.Context(), cfg)
	if err != nil || got.ContextManifest.BudgetPlan == nil || got.ContextManifest.BudgetPlan.HistoryBudget != 998 {
		t.Fatalf("admission error=%v plan=%+v", err, got.ContextManifest.BudgetPlan)
	}
	if countMessagesWithText(got.Messages, "continue") != 1 || countMessagesWithText(got.Messages, strings.Repeat("h", 100)) != 0 {
		t.Fatalf("over-budget history must be trimmed while the current input stays: %+v", got.Messages)
	}
}

func TestProviderBudgetKeepsAuditWhenProtectedHistoryOverflows(t *testing.T) {
	cfg := providerBudgetFixture()
	got, err := ProviderRunConfigApplier(nil)(t.Context(), cfg)
	if !errors.Is(err, contextfrag.ErrProtectedContextOverflow) || got.ContextManifest.BudgetPlan == nil {
		t.Fatalf("protected overflow lost budget error or plan: %v / %+v", err, got.ContextManifest.BudgetPlan)
	}
	found := false
	for _, mutation := range got.ContextMutations.Records() {
		found = found || mutation.Kind == contextfrag.MutationContextBudgetFailure
	}
	if !found {
		t.Fatal("unresolved overflow must retain failed-budget evidence")
	}
}

func countMessagesWithText(messages []sdk.Message, text string) int {
	count := 0
	for _, message := range messages {
		for _, part := range message.Content {
			if textPart, ok := part.(sdk.TextPart); ok && strings.Contains(textPart.Text, text) {
				count++
			}
		}
	}
	return count
}
