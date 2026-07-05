package contextview

import (
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestFragBudgetDropsOversizedFragEvenWhenEnvelopeBudgetIsSufficient(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	oversized := messageFrag("oversized", sdk.AssistantMessage(strings.Repeat("x", 400)))
	oversized.Budget = contextfrag.BudgetPolicy{MaxTokens: 1, Overflow: contextfrag.OverflowDrop}
	frags := []contextfrag.ContextFrag{
		messageFrag("plain", sdk.AssistantMessage("plain content")),
		oversized,
	}

	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{MaxTokens: 100000})

	for _, frag := range result.Selected {
		if frag.ID == "oversized" {
			t.Fatal("oversized frag with OverflowDrop must not be selected")
		}
	}
	var reason string
	for _, record := range result.Summary.DropReasons {
		if record.FragID == "oversized" {
			reason = record.Reason
		}
	}
	if !strings.HasPrefix(reason, "frag_budget:") {
		t.Fatalf("drop reason = %q, want frag_budget: prefix", reason)
	}
}

func TestFragBudgetTrimsPureTextFragOverMaxChars(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	long := textFrag("long-note", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, strings.Repeat("a", 50))
	long.Budget = contextfrag.BudgetPolicy{MaxChars: 10, Overflow: contextfrag.OverflowTrim}

	result := selector.Select([]contextfrag.ContextFrag{long}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 {
		t.Fatalf("selected = %d, want 1", len(result.Selected))
	}
	text := result.Selected[0].Parts[0].Text
	if !strings.HasPrefix(text, strings.Repeat("a", 10)) {
		t.Fatalf("trimmed text = %q, want to keep the first 10 chars", text)
	}
	if !strings.Contains(text, "[trimmed:") {
		t.Fatalf("trimmed text = %q, want a trim marker", text)
	}
	if len(result.Edited) == 0 {
		t.Fatal("trim must record an edit trace")
	}
}

func TestFragBudgetSummarizeUnsupportedKeepsAndWarns(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frag := textFrag("note", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, strings.Repeat("b", 50))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 10, Overflow: contextfrag.OverflowSummarize}

	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 || result.Selected[0].ID != "note" {
		t.Fatalf("summarize-unsupported frag must be kept: selected=%#v", result.Selected)
	}
	if result.Selected[0].Parts[0].Text != strings.Repeat("b", 50) {
		t.Fatal("summarize-unsupported frag content must be untouched")
	}
	var found bool
	for _, w := range result.Warnings {
		if w.Code == "overflow_summarize_unsupported" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected overflow_summarize_unsupported warning, got %#v", result.Warnings)
	}
}

func TestFragBudgetNoTriggerLeavesFragUntouched(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	withinLimit := textFrag("within", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, "short")
	withinLimit.Budget = contextfrag.BudgetPolicy{MaxChars: 1000, Overflow: contextfrag.OverflowDrop}
	noLimit := textFrag("nolimit", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, strings.Repeat("c", 500))
	frags := []contextfrag.ContextFrag{withinLimit, noLimit}

	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 2 {
		t.Fatalf("selected = %d, want 2 (no policy should trigger)", len(result.Selected))
	}
	if len(result.Dropped) != 0 || len(result.Edited) != 0 || len(result.Warnings) != 0 {
		t.Fatalf("no side effects expected: dropped=%d edited=%d warnings=%d", len(result.Dropped), len(result.Edited), len(result.Warnings))
	}
}

func TestFragBudgetDropProtectsToolClosure(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	bulky := toolResultFrag("result", "calc", "call-1", strings.Repeat("r", 400))
	bulky.Budget = contextfrag.BudgetPolicy{MaxTokens: 1, Overflow: contextfrag.OverflowDrop}
	frags := []contextfrag.ContextFrag{
		toolCallFrag("call", "calc", "call-1"),
		bulky,
	}

	got := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	var kept bool
	for _, frag := range got.Selected {
		if frag.ID == "result" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("tool-result frag must survive OverflowDrop to protect the tool closure")
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Code != "frag_budget_drop_blocked_tool_closure" {
		t.Fatalf("closure-protected drop must record a validation warning: %#v", got.Warnings)
	}
}

func TestFragBudgetTrimUnsupportedOnNonPureTextKeepsAndWarns(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frag := messageFrag("assistant-note", sdk.AssistantMessage(strings.Repeat("d", 50)))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 10, Overflow: contextfrag.OverflowTrim}

	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 || result.Selected[0].ID != "assistant-note" {
		t.Fatalf("non-pure-text frag must be kept when Trim is unsupported: selected=%#v", result.Selected)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "overflow_trim_unsupported" {
		t.Fatalf("expected overflow_trim_unsupported warning, got %#v", result.Warnings)
	}
}

func TestFragBudgetOverflowKeepIgnoresLimits(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frag := textFrag("pinned", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, strings.Repeat("e", 50))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 1, Overflow: contextfrag.OverflowKeep}

	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 || result.Selected[0].Parts[0].Text != strings.Repeat("e", 50) {
		t.Fatalf("OverflowKeep must ignore MaxChars entirely: selected=%#v", result.Selected)
	}
	if len(result.Warnings) != 0 || len(result.Dropped) != 0 || len(result.Edited) != 0 {
		t.Fatalf("OverflowKeep must have zero side effects: warnings=%d dropped=%d edited=%d", len(result.Warnings), len(result.Dropped), len(result.Edited))
	}
}
