package contextview

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestACPSelector_ProfileMustKeepSystemAndCurrentUser(t *testing.T) {
	t.Parallel()

	profile := (&FragmentSelector{}).ProfileFor(contextfrag.IntentACPRuntimePrompt)

	if profile.Intent != contextfrag.IntentACPRuntimePrompt {
		t.Fatalf("Intent = %q, want %q", profile.Intent, contextfrag.IntentACPRuntimePrompt)
	}
	if !slotInProfile(profile, contextfrag.SlotSystem) {
		t.Fatalf("MustKeepSlots = %#v, want system", profile.MustKeepSlots)
	}
	if !slotInProfile(profile, contextfrag.SlotCurrentUser) {
		t.Fatalf("MustKeepSlots = %#v, want current_user", profile.MustKeepSlots)
	}
}

func TestACPSelector_SelectRetainsAllForACPIntent(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("old-assistant", sdk.AssistantMessage("old answer")),
		textFrag("current", contextfrag.SlotCurrentUser, contextfrag.KindCurrentUserMessage, sdk.MessageRoleUser, "latest question"),
	}
	selector := &FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentACPRuntimePrompt)

	result := selector.Select(frags, profile, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"system", "old-user", "old-assistant", "current"})
	if len(result.Dropped) != 0 {
		t.Fatalf("dropped = %#v, want none", fragIDs(result.Dropped))
	}
}
