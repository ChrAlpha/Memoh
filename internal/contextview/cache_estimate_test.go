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
