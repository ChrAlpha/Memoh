package contextview

import (
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func TestStablePrefixPlacerMarksContiguousStablePrefix(t *testing.T) {
	t.Parallel()

	dynamic := textFrag("dynamic", contextfrag.SlotSystem, contextfrag.KindToolUsage, sdk.MessageRoleSystem, "runtime tools")
	dynamic.CacheClass = contextfrag.CacheDynamic
	frags := contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{
		textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		textFrag("identity", contextfrag.SlotSystem, contextfrag.KindBotIdentity, sdk.MessageRoleSystem, "identity"),
		dynamic,
		messageFrag("latest", sdk.UserMessage("latest")),
	})

	plan := StablePrefixPlacer{}.Place(frags, contextfrag.IntentRunConfigPreProvider)

	if plan.FirstVolatileIndex != 2 {
		t.Fatalf("FirstVolatileIndex = %d, want 2", plan.FirstVolatileIndex)
	}
	if len(plan.Items) != len(frags) {
		t.Fatalf("items = %d, want %d", len(plan.Items), len(frags))
	}
	for i, item := range plan.Items {
		if item.Position != i || item.FragID != frags[i].ID || item.CacheHint != frags[i].CacheClass {
			t.Fatalf("item %d = %#v, want position/id/cache from frag %#v", i, item, frags[i])
		}
	}
}

func TestStablePrefixPlacerKeepsSystemHistoryMessagesStable(t *testing.T) {
	t.Parallel()

	sessionEvent := messageFrag("event", sdk.Message{
		Role:    sdk.MessageRoleSystem,
		Content: []sdk.MessagePart{sdk.TextPart{Text: `<event type="session_created" t="2026-08-26T21:14:40Z"/>`}},
	})
	sessionEvent.CacheClass = cacheForSDKMessage(sdk.Message{Role: sdk.MessageRoleSystem})
	current := messageFrag("current", sdk.UserMessage("latest"))
	current.CacheClass = contextfrag.CacheNever
	frags := contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{
		textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		sessionEvent,
		messageFrag("turn1", sdk.UserMessage("hello")),
		current,
	})

	plan := StablePrefixPlacer{}.Place(frags, contextfrag.IntentRunConfigPreProvider)

	if plan.FirstVolatileIndex != 3 {
		t.Fatalf("FirstVolatileIndex = %d, want 3 (session event and history stay in the stable prefix)", plan.FirstVolatileIndex)
	}
}
