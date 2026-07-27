package contextview

import (
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestStablePrefixTokenEstimateCoversFullCacheablePrefix(t *testing.T) {
	t.Parallel()

	system := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "system.prompt", Kind: contextfrag.KindSystemPrompt, Slot: contextfrag.SlotSystem,
		CacheClass: contextfrag.CacheStable, Text: strings.Repeat("s", 400),
	})
	stableMsg := contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:      "history.db_message.m1",
		Message: sdk.Message{Role: sdk.MessageRoleUser, Content: []sdk.MessagePart{sdk.TextPart{Text: strings.Repeat("h", 800)}}},
		Kind:    contextfrag.KindConversationEvent,
		Slot:    contextfrag.SlotHistory,
	})
	volatile := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "current_user.message", Kind: contextfrag.KindCurrentUserMessage, Slot: contextfrag.SlotCurrentUser,
		CacheClass: contextfrag.CacheNever, Text: "now",
	})
	selected := []contextfrag.ContextFrag{system, stableMsg, volatile}
	placement := PlacementPlan{
		Items: []PlacementItem{
			{FragID: system.ID, Slot: system.Slot, CacheHint: contextfrag.CacheStable},
			{FragID: stableMsg.ID, Slot: stableMsg.Slot, CacheHint: contextfrag.CacheStable},
			{FragID: volatile.ID, Slot: volatile.Slot, CacheHint: contextfrag.CacheNever},
		},
		FirstVolatileIndex: 2,
	}
	toolDefs := []contextfrag.ToolDefAccounting{
		{Provider: "native", Name: "send_message", Bytes: 400, TokenEstimate: 100},
		{Provider: "mcp", Name: "jira_search", Bytes: 800, TokenEstimate: 200},
	}

	got := stablePrefixTokenEstimate(placement, selected, toolDefs)
	want := contextfrag.ResolveFragTokens(system) + contextfrag.ResolveFragTokens(stableMsg) + 300
	if got != want {
		t.Fatalf("stablePrefixTokenEstimate = %d, want %d", got, want)
	}
}

func TestStablePrefixTokenEstimateEmptyPlacement(t *testing.T) {
	t.Parallel()

	if got := stablePrefixTokenEstimate(PlacementPlan{}, nil, nil); got != 0 {
		t.Fatalf("stablePrefixTokenEstimate(empty) = %d, want 0", got)
	}
}

func stableSpanFixture(counts []int) (PlacementPlan, []contextfrag.ContextFrag) {
	system := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: "system.prompt", Kind: contextfrag.KindSystemPrompt, Slot: contextfrag.SlotSystem,
		CacheClass: contextfrag.CacheStable, Text: strings.Repeat("s", 400),
	})
	placement := PlacementPlan{Items: []PlacementItem{{FragID: system.ID, Slot: system.Slot, CacheHint: contextfrag.CacheStable}}}
	selected := []contextfrag.ContextFrag{system}
	for i, tokens := range counts {
		frag := contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:      "history.db_message.m" + string(rune('1'+i)),
			Message: sdk.Message{Role: sdk.MessageRoleUser, Content: []sdk.MessagePart{sdk.TextPart{Text: "x"}}},
			Kind:    contextfrag.KindConversationEvent,
			Slot:    contextfrag.SlotHistory,
		})
		frag.TokenEstimate = tokens
		placement.Items = append(placement.Items, PlacementItem{FragID: frag.ID, Slot: frag.Slot, CacheHint: contextfrag.CacheStable})
		selected = append(selected, frag)
	}
	placement.FirstVolatileIndex = len(placement.Items)
	return placement, selected
}

func TestMidStableMessageCountSplitsLargeSpans(t *testing.T) {
	t.Parallel()

	placement, selected := stableSpanFixture([]int{1000, 1000, 1000, 1000})
	if got := midStableMessageCount(placement, selected); got != 2 {
		t.Fatalf("midStableMessageCount = %d, want 2 (half of the 4000-token span)", got)
	}
}

func TestMidStableMessageCountSkipsSmallSpans(t *testing.T) {
	t.Parallel()

	placement, selected := stableSpanFixture([]int{300, 300})
	if got := midStableMessageCount(placement, selected); got != 0 {
		t.Fatalf("midStableMessageCount = %d, want 0 for spans below the insurance threshold", got)
	}
}

func TestMidStableMessageCountNeverEqualsFullSpan(t *testing.T) {
	t.Parallel()

	placement, selected := stableSpanFixture([]int{4000})
	if got := midStableMessageCount(placement, selected); got != 0 {
		t.Fatalf("midStableMessageCount = %d, want 0 when the midpoint would duplicate the tail breakpoint", got)
	}
}
