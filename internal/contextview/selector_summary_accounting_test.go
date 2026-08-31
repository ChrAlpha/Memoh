package contextview

import (
	"fmt"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func TestConversationSummarySurvivesHistoryBudgetTrim(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentRunConfigPreProvider)

	frags := []contextfrag.ContextFrag{contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "summary", Kind: contextfrag.KindConversationSummary, Role: sdk.MessageRoleUser,
		Slot: contextfrag.SlotHistory, Text: strings.Repeat("s", 2_000),
		Trust: contextfrag.TrustExternal,
	})}
	for i := range 20 {
		frags = append(frags, contextfrag.TextFrag(contextfrag.TextFragInput{
			ID: fmt.Sprintf("raw-%d", i), Kind: contextfrag.KindConversationEvent,
			Role: sdk.MessageRoleUser, Slot: contextfrag.SlotHistory,
			Text: strings.Repeat("r", 2_000), Trust: contextfrag.TrustExternal,
		}))
	}

	result := selector.Select(frags, profile, BudgetEnvelope{
		Plan: &contextfrag.ContextBudgetPlan{SystemBudget: 6_000},
	})

	if result.FatalError != nil {
		t.Fatalf("Select() fatal = %v", result.FatalError)
	}
	if !containsFragID(result.Selected, "summary") {
		t.Fatalf("summary evicted under history-budget pressure: dropped=%v", fragIDs(result.Dropped))
	}
	if !containsFragID(result.Selected, "raw-19") {
		t.Fatalf("newest raw row missing: selected=%v", fragIDs(result.Selected))
	}
	if !containsFragID(result.Dropped, "raw-0") {
		t.Fatalf("oldest raw row must fund the trim: dropped=%v", fragIDs(result.Dropped))
	}
}
