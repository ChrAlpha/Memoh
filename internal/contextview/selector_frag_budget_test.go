package contextview

import (
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
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
	original := strings.Repeat("a", 50)
	long := textFrag("long-note", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, original)
	long.Budget = contextfrag.BudgetPolicy{MaxChars: 30, Overflow: contextfrag.OverflowTrim}

	result := selector.Select([]contextfrag.ContextFrag{long}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 {
		t.Fatalf("selected = %d, want 1", len(result.Selected))
	}
	text := result.Selected[0].Parts[0].Text
	if !strings.HasPrefix(text, strings.Repeat("a", 7)) {
		t.Fatalf("trimmed text = %q, want to keep the first 7 chars", text)
	}
	if !strings.Contains(text, "[trimmed from 50 bytes]") {
		t.Fatalf("trimmed text = %q, want a trim marker naming the original size", text)
	}
	if got := utf8.RuneCountInString(text); got > long.Budget.MaxChars {
		t.Fatalf("trimmed text rune length = %d, want <= MaxChars (%d)", got, long.Budget.MaxChars)
	}
	if len(result.Edited) == 0 {
		t.Fatal("trim must record an edit trace")
	}
}

func TestFragBudgetTrimRefreshesAuditHashWithinSameTokenBucket(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	source := textFrag(
		"same-bucket",
		contextfrag.SlotHistory,
		contextfrag.KindConversationEvent,
		sdk.MessageRoleAssistant,
		"1234567",
	)
	source.Budget = contextfrag.BudgetPolicy{MaxChars: 6, Overflow: contextfrag.OverflowTrim}
	source = contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{source})[0]

	result := selector.Select(
		[]contextfrag.ContextFrag{source},
		selector.ProfileFor(contextfrag.IntentRunConfigPreProvider),
		BudgetEnvelope{},
	)
	decisions := selectionDecisions([]contextfrag.ContextFrag{source}, result)

	if len(decisions) != 1 || decisions[0].Decision != contextfrag.DecisionTrimmed {
		t.Fatalf("selection decisions = %#v, want one trimmed decision", decisions)
	}
	if decisions[0].Ref.ContentHash == source.Ref.ContentHash {
		t.Fatalf("trim retained stale content hash %q", decisions[0].Ref.ContentHash)
	}
	if got, want := decisions[0].TokenEstimate, contextfrag.ResolveFragTokens(result.Selected[0]); got != want {
		t.Fatalf("decision token estimate = %d, want refreshed selected estimate %d", got, want)
	}
}

func TestFragBudgetDropsOptionalSystemBeforeTierPass(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	optional := textFrag(
		"system.optional",
		contextfrag.SlotSystem,
		contextfrag.KindSystemPrompt,
		sdk.MessageRoleSystem,
		"oversized optional section",
	)
	optional.Trust = contextfrag.TrustSystem
	optional.RetentionTier = contextfrag.RetentionOptional
	optional.Budget = contextfrag.BudgetPolicy{MaxTokens: 1, Overflow: contextfrag.OverflowDrop}
	plan := &contextfrag.ContextBudgetPlan{SystemBudget: 500}

	result := selector.Select(
		[]contextfrag.ContextFrag{optional},
		selector.ProfileFor(contextfrag.IntentRunConfigPreProvider),
		BudgetEnvelope{Plan: plan},
	)

	if len(result.Selected) != 0 || len(result.Dropped) != 1 {
		t.Fatalf("selection = %#v, want optional oversized system fragment dropped", result)
	}
	if len(result.Summary.DropReasons) != 1 ||
		result.Summary.DropReasons[0].Reason != "frag_budget:max_tokens" {
		t.Fatalf("drop reasons = %#v, want per-fragment budget reason", result.Summary.DropReasons)
	}
}

func TestFragBudgetTrimRespectsMaxTokensBudgetIncludingMarker(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	original := strings.Repeat("z", 200)
	long := textFrag("long-tool-note", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, original)
	long.Budget = contextfrag.BudgetPolicy{MaxTokens: 10, Overflow: contextfrag.OverflowTrim}

	result := selector.Select([]contextfrag.ContextFrag{long}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 {
		t.Fatalf("selected = %d, want 1", len(result.Selected))
	}
	text := result.Selected[0].Parts[0].Text
	if !strings.Contains(text, "[trimmed from 200 bytes]") {
		t.Fatalf("trimmed text = %q, want a trim marker naming the original size", text)
	}
	maxBytes := long.Budget.MaxTokens * fragBudgetTokenByteFactor
	if got := len(text); got > maxBytes {
		t.Fatalf("trimmed text byte length = %d, want <= MaxTokens*%d (%d)", got, fragBudgetTokenByteFactor, maxBytes)
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

func TestFragBudgetCountsNonTextToolResultContentForMaxChars(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	bulky := toolResultFrag("result", "calc", "call-1", strings.Repeat("r", 400))
	bulky.Budget = contextfrag.BudgetPolicy{MaxChars: 5, Overflow: contextfrag.OverflowDrop}
	frags := []contextfrag.ContextFrag{
		toolCallFrag("call", "calc", "call-1"),
		bulky,
	}

	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Warnings) != 1 || result.Warnings[0].Code != "frag_budget_drop_blocked_tool_closure" {
		t.Fatalf("expected frag_budget_drop_blocked_tool_closure once MaxChars accounts for non-text tool-result content, got %#v", result.Warnings)
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

func TestFragBudgetTrimHonorsMaxTokensWhenMaxCharsAlsoConfigured(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	original := strings.Repeat("m", 100)
	frag := textFrag("note", contextfrag.SlotHistory, contextfrag.KindConversationEvent, sdk.MessageRoleAssistant, original)
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 1000, MaxTokens: 5, Overflow: contextfrag.OverflowTrim}

	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	if len(result.Selected) != 1 {
		t.Fatalf("selected = %d, want 1", len(result.Selected))
	}
	text := result.Selected[0].Parts[0].Text
	maxBytes := frag.Budget.MaxTokens * fragBudgetTokenByteFactor
	if got := len(text); got > maxBytes {
		t.Fatalf("trimmed text byte length = %d, want <= MaxTokens*%d (%d)", got, fragBudgetTokenByteFactor, maxBytes)
	}
	if text == original {
		t.Fatal("expected the MaxTokens dimension to actually truncate the content even though MaxChars did not trigger")
	}
	for _, w := range result.Warnings {
		if w.Code == "overflow_trim_unsupported" {
			t.Fatalf("did not expect overflow_trim_unsupported once both budget dimensions are enforced: %#v", result.Warnings)
		}
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

func TestFragBudgetDropProtectsMustKeepSlot(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frag := textFrag("current-user", contextfrag.SlotCurrentUser, contextfrag.KindConversationEvent, sdk.MessageRoleUser, strings.Repeat("u", 400))
	frag.Budget = contextfrag.BudgetPolicy{MaxChars: 1, Overflow: contextfrag.OverflowDrop}

	result := selector.Select([]contextfrag.ContextFrag{frag}, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	var kept bool
	for _, f := range result.Selected {
		if f.ID == "current-user" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("must-keep SlotCurrentUser frag must survive OverflowDrop")
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "frag_budget_drop_blocked_must_keep" {
		t.Fatalf("must-keep drop must record a validation warning: %#v", result.Warnings)
	}
}
