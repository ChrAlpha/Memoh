package contextview

import (
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestFragBudgetDropsOversizedDroppableFragment(t *testing.T) {
	t.Parallel()
	selector := &FragmentSelector{}
	frag := textFrag("oversized", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, strings.Repeat("x", 40))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 5, Overflow: contextfrag.OverflowDrop}
	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})
	if len(result.Selected) != 0 || len(result.Dropped) != 1 || result.Summary.DropReasons[0].Reason != "frag_budget:max_chars" {
		t.Fatalf("result = %#v", result)
	}
}

func TestFragBudgetTrimRefreshesAccountingAndHash(t *testing.T) {
	t.Parallel()
	selector := &FragmentSelector{}
	frag := textFrag("trimmed", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, strings.Repeat("z", 80))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 30, Overflow: contextfrag.OverflowTrim}
	frag.TokenEstimate = 999
	frag = contextfrag.WithContextRef(frag, frag.Ref)
	originalHash := frag.Ref.ContentHash
	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})
	if len(result.Selected) != 1 || len(result.Edited) != 1 {
		t.Fatalf("result = %#v", result)
	}
	trimmed := result.Selected[0]
	if trimmed.TokenEstimate != 0 {
		t.Fatalf("token estimate = %d, want recomputation", trimmed.TokenEstimate)
	}
	if trimmed.Ref.ContentHash == "" || trimmed.Ref.ContentHash == originalHash {
		t.Fatalf("content hash = %q, original = %q", trimmed.Ref.ContentHash, originalHash)
	}
	if len(trimmed.Parts[0].Text) > frag.Budget.MaxChars {
		t.Fatalf("trimmed text = %q", trimmed.Parts[0].Text)
	}
}

func TestFragBudgetKeepsMustKeepSlotAndWarns(t *testing.T) {
	t.Parallel()
	selector := &FragmentSelector{}
	frag := textFrag("current", contextfrag.SlotCurrentUser, contextfrag.KindCurrentUserMessage, sdk.MessageRoleUser, strings.Repeat("u", 40))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 1, Overflow: contextfrag.OverflowDrop}
	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})
	if len(result.Selected) != 1 || len(result.Warnings) != 1 || result.Warnings[0].Code != "frag_budget_drop_blocked_must_keep" {
		t.Fatalf("result = %#v", result)
	}
}
