package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func conflictFrag(id, key, text string, trust contextfrag.TrustLevel, scope contextfrag.Scope) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:          id,
		Kind:        contextfrag.KindSystemPolicy,
		Role:        sdk.MessageRoleSystem,
		Slot:        contextfrag.SlotSystem,
		Text:        text,
		Trust:       trust,
		Scope:       scope,
		Source:      "test",
		Collector:   "test",
		ConflictKey: key,
	})
}

func TestConflictGroupClosestScopeWins(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frags := []contextfrag.ContextFrag{
		conflictFrag("policy.bot", "policy", "bot-wide rule", contextfrag.TrustSystem, contextfrag.Scope{BotID: "b"}),
		conflictFrag("policy.session", "policy", "session rule", contextfrag.TrustSystem, contextfrag.Scope{BotID: "b", ChatID: "c", SessionID: "s"}),
		conflictFrag("other", "", "unrelated", contextfrag.TrustSystem, contextfrag.Scope{BotID: "b"}),
	}
	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	ids := map[string]bool{}
	for _, frag := range result.Selected {
		ids[frag.ID] = true
	}
	if !ids["policy.session"] || ids["policy.bot"] {
		t.Fatalf("closest scope must win the conflict group: %v", ids)
	}
	if !ids["other"] {
		t.Fatal("fragments without a conflict key are untouched")
	}
	var reason string
	for _, record := range result.Summary.DropReasons {
		if record.FragID == "policy.bot" {
			reason = record.Reason
		}
	}
	if !strings.HasPrefix(reason, "precedence:") {
		t.Fatalf("superseded fragment must carry a precedence reason, got %q", reason)
	}
}

func TestConflictGroupTrustBreaksScopeTie(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	scope := contextfrag.Scope{BotID: "b", SessionID: "s"}
	frags := []contextfrag.ContextFrag{
		conflictFrag("id.workspace", "identity", "from workspace", contextfrag.TrustWorkspace, scope),
		conflictFrag("id.system", "identity", "from system", contextfrag.TrustSystem, scope),
	}
	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})
	if len(result.Selected) != 1 || result.Selected[0].ID != "id.system" {
		t.Fatalf("higher trust must break the scope tie: %+v", result.Selected)
	}
}

func TestConflictGroupLaterCollectionWinsFullTie(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	scope := contextfrag.Scope{BotID: "b"}
	frags := []contextfrag.ContextFrag{
		conflictFrag("usage.stale", "system.tool_usage", "stale usage", contextfrag.TrustSystem, scope),
		conflictFrag("usage.fresh", "system.tool_usage", "fresh usage", contextfrag.TrustSystem, scope),
	}
	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})
	if len(result.Selected) != 1 || result.Selected[0].ID != "usage.fresh" {
		t.Fatalf("on a full tie the later-collected fragment wins: %+v", result.Selected)
	}
}

func TestTrustFloorRejectsUserTrustFromSystemSlot(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	frags := []contextfrag.ContextFrag{
		conflictFrag("system.ok", "", "real system", contextfrag.TrustSystem, contextfrag.Scope{BotID: "b"}),
		conflictFrag("system.smuggled", "", "user-authored", contextfrag.TrustUser, contextfrag.Scope{BotID: "b"}),
	}
	result := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})

	for _, frag := range result.Selected {
		if frag.ID == "system.smuggled" {
			t.Fatal("user-trust content must not enter the provider system slot")
		}
	}
	var reason string
	for _, record := range result.Summary.DropReasons {
		if record.FragID == "system.smuggled" {
			reason = record.Reason
		}
	}
	if !strings.HasPrefix(reason, "trust_gate:") {
		t.Fatalf("trust gate drop must carry a trust_gate reason, got %q", reason)
	}
}

func TestFragsFirstDedupesAgentToolUsage(t *testing.T) {
	t.Parallel()

	cfg := fragsFirstFixture()
	cfg.ContextSourceFrags = append(cfg.ContextSourceFrags, ToolUsageFrag("stale embedded usage", cfg.ContextScope))
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if strings.Contains(got.System, "stale embedded usage") {
		t.Fatalf("agent-supplied tool usage must supersede the embedded one: %q", got.System)
	}
	if !strings.Contains(got.System, "USE_TOOLS") {
		t.Fatalf("agent tool usage missing from system: %q", got.System)
	}
	var usageFrags int
	for _, frag := range got.ContextFrags {
		if frag.Kind == contextfrag.KindToolUsage {
			usageFrags++
		}
	}
	if usageFrags != 1 {
		t.Fatalf("tool usage frags = %d, want exactly one after conflict resolution", usageFrags)
	}
}
