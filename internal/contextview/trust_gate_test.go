package contextview

import (
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func trustGateFrag(id string, slot contextfrag.Slot, trust contextfrag.TrustLevel) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:        id,
		Kind:      contextfrag.KindSystemPrompt,
		Role:      sdk.MessageRoleSystem,
		Slot:      slot,
		Text:      "content of " + id,
		Trust:     trust,
		Scope:     contextfrag.Scope{BotID: "bot-1"},
		Source:    "test",
		Collector: "test",
	})
}

func TestTrustGateDropsExternalSystemFragsForProviderIntent(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frags := []contextfrag.ContextFrag{
		trustGateFrag("system.prompt", contextfrag.SlotSystem, contextfrag.TrustSystem),
		trustGateFrag("system.injected", contextfrag.SlotSystem, contextfrag.TrustExternal),
		trustGateFrag("history.msg", contextfrag.SlotHistory, contextfrag.TrustExternal),
	}
	profile := selector.ProfileFor(contextfrag.IntentRunConfigPreProvider)
	result := selector.Select(frags, profile, BudgetEnvelope{})

	for _, frag := range result.Selected {
		if frag.ID == "system.injected" {
			t.Fatal("external-trust fragment must not be selected into the system slot")
		}
	}
	if len(result.Selected) != 2 {
		t.Fatalf("selected = %d, want system prompt and history kept", len(result.Selected))
	}
	var gated bool
	for _, record := range result.Summary.DropReasons {
		if record.FragID == "system.injected" && strings.Contains(record.Reason, "trust_gate") {
			gated = true
		}
	}
	if !gated {
		t.Fatalf("gate drop must be recorded with a trust_gate reason: %+v", result.Summary.DropReasons)
	}
}

func TestTrustGateExemptsACPRuntimePrompt(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frags := []contextfrag.ContextFrag{
		trustGateFrag("acp.preamble", contextfrag.SlotSystem, contextfrag.TrustSystem),
		trustGateFrag("acp.section.attachments", contextfrag.SlotSystem, contextfrag.TrustExternal),
	}
	profile := selector.ProfileFor(contextfrag.IntentACPRuntimePrompt)
	result := selector.Select(frags, profile, BudgetEnvelope{})

	if len(result.Selected) != 2 {
		t.Fatalf("ACP document sections must not be trust-gated, selected = %d", len(result.Selected))
	}
}
