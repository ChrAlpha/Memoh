package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

func TestApplyProviderRunConfigGatesUnavailableSkillGuidance(t *testing.T) {
	t.Parallel()

	cfg := capabilityGateFixture()
	got := applyProviderRunConfigOK(context.Background(), nil, cfg)

	if got.System != "base system" {
		t.Fatalf("system = %q, want unavailable skill guidance removed", got.System)
	}
	if strings.Contains(got.System, "[System Notice]") {
		t.Fatalf("capability gating must not render an omission marker: %q", got.System)
	}
	for _, id := range []string{"system.skills.header", "system.skill.alpha"} {
		decision, ok := decisionByID(got.ContextManifest.SelectionDecisions, id)
		if !ok || decision.Decision != contextfrag.DecisionDropped || decision.Reason != capabilityGateDropReason {
			t.Errorf("decision for %q = %#v, want dropped/%s", id, decision, capabilityGateDropReason)
		}
	}
	if got.ContextManifest.Selection == nil ||
		got.ContextManifest.Selection.DropReasons[capabilityGateDropReason] != 2 {
		t.Fatalf("selection = %#v, want two capability-gated drops", got.ContextManifest.Selection)
	}
	records := got.ContextMutations.Records()
	if len(records) != 1 ||
		records[0].Kind != contextfrag.MutationCapabilityGate ||
		records[0].Detail != "dropped=2" {
		t.Fatalf("mutations = %+v, want one count-only capability gate record", records)
	}
}

func TestApplyProviderRunConfigKeepsAvailableSkillGuidanceByteIdentical(t *testing.T) {
	t.Parallel()

	cfg := capabilityGateFixture()
	cfg.ContextToolDefs = []contextfrag.ToolDefAccounting{{Name: tools.ToolUseSkill().String()}}
	got := applyProviderRunConfigOK(context.Background(), nil, cfg)

	if got.System != cfg.System {
		t.Fatalf("system = %q, want byte-identical %q", got.System, cfg.System)
	}
	if got.ContextManifest.Selection != nil &&
		got.ContextManifest.Selection.DropReasons[capabilityGateDropReason] != 0 {
		t.Fatalf("selection = %#v, want no capability gate", got.ContextManifest.Selection)
	}
	if records := got.ContextMutations.Records(); len(records) != 0 {
		t.Fatalf("mutations = %+v, want none", records)
	}
}

func TestApplyProviderRunConfigCapabilityGateKeepsHeaderForRemainingToolUsage(t *testing.T) {
	t.Parallel()

	base := capabilitySystemFrag(
		"system.prompt",
		"base system",
		contextfrag.KindSystemPrompt,
		20,
		"",
		contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
	)
	cfg := agentpkg.RunConfig{
		System:             "base system\n\n## Tool usage\n\nZeta usage\n\nAlpha 用法",
		ContextSourceFrags: []contextfrag.ContextFrag{base},
		ContextToolUsage:   "## Tool usage\n\nZeta usage\n\nAlpha 用法",
		ContextToolUsageFrags: []contextfrag.ContextFrag{
			toolUsageTestFrag("system.tool_usage.header", "## Tool usage", "zeta_tool", 0),
			toolUsageTestFrag("system.tool_usage.zeta_tool", "Zeta usage", "zeta_tool", 1),
			toolUsageTestFrag("system.tool_usage.alpha_tool", "Alpha 用法", "alpha_tool", 2),
		},
		ContextToolDefs:         []contextfrag.ToolDefAccounting{{Name: "alpha_tool"}},
		ContextToolDefsResolved: true,
	}

	got := applyProviderRunConfigOK(context.Background(), nil, cfg)
	want := "base system\n\n## Tool usage\n\nAlpha 用法"
	if got.System != want {
		t.Fatalf("system = %q, want remaining provider usage %q", got.System, want)
	}
	if !hasFragID(got.ContextFrags, "system.tool_usage.header") ||
		hasFragID(got.ContextFrags, "system.tool_usage.zeta_tool") {
		t.Fatalf("selected fragments = %v, want header plus only available provider", fragIDs(got.ContextFrags))
	}
	if decision, ok := decisionByID(
		got.ContextManifest.SelectionDecisions,
		"system.tool_usage.zeta_tool",
	); !ok || decision.Reason != capabilityGateDropReason {
		t.Fatalf("zeta decision = %#v, want capability-gated", decision)
	}
}

func TestApplyProviderRunConfigLeavesLegacySystemUngated(t *testing.T) {
	t.Parallel()

	cfg := capabilityGateFixture()
	cfg.ContextSourceFrags = nil
	got := applyProviderRunConfigOK(context.Background(), nil, cfg)
	if got.System != cfg.System {
		t.Fatalf("legacy system = %q, want unchanged %q", got.System, cfg.System)
	}
}

func TestApplyProviderRunConfigFallbackCannotRestoreGatedGuidance(t *testing.T) {
	t.Parallel()

	cfg := capabilityGateFixture()
	cfg.Messages = []sdk.Message{sdk.UserMessage("legacy message")}
	cfg.ContextSourceFrags = append(
		cfg.ContextSourceFrags,
		contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:        "duplicate",
			Message:   sdk.UserMessage("first"),
			Kind:      contextfrag.KindConversationEvent,
			Slot:      contextfrag.SlotHistory,
			Source:    contextfrag.SourceRunConfig,
			Collector: sourceFragsCollectorName,
		}),
		contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:        "duplicate",
			Message:   sdk.AssistantMessage("second"),
			Kind:      contextfrag.KindConversationEvent,
			Slot:      contextfrag.SlotHistory,
			Source:    contextfrag.SourceRunConfig,
			Collector: sourceFragsCollectorName,
		}),
	)

	got, err := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig error = %v, want ordinary fallback", err)
	}
	if got.System != "base system" || strings.Contains(got.System, "alpha") {
		t.Fatalf("fallback system = %q, want capability-filtered base system", got.System)
	}
	if len(got.Messages) != 1 || messageText(t, got.Messages[0]) != "legacy message" {
		t.Fatalf("fallback messages = %#v, want legacy message", got.Messages)
	}
	records := got.ContextMutations.Records()
	if len(records) != 2 ||
		records[0].Kind != contextfrag.MutationCapabilityGate ||
		records[1].Kind != contextfrag.MutationContextViewFallback {
		t.Fatalf("fallback mutations = %+v, want capability gate then context-view fallback", records)
	}
}

func capabilityGateFixture() agentpkg.RunConfig {
	capability := tools.ToolUseSkill().String()
	render := contextfrag.RenderPolicy{
		Format:      contextfrag.RenderMarkdown,
		GroupID:     "system.skills",
		GroupJoiner: "\n",
	}
	frags := []contextfrag.ContextFrag{
		capabilitySystemFrag(
			"system.prompt",
			"base system",
			contextfrag.KindSystemPrompt,
			20,
			"",
			contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		),
		capabilitySystemFrag(
			"system.skills.header",
			"## Skills\n\n1 skill(s) available:",
			contextfrag.KindSkillsCatalog,
			40,
			capability,
			render,
		),
		capabilitySystemFrag(
			"system.skill.alpha",
			"- **alpha**: Alpha 用法",
			contextfrag.KindSkillsCatalog,
			40,
			capability,
			render,
		),
	}
	return agentpkg.RunConfig{
		System:                  contextfrag.Render(frags).System,
		ContextSourceFrags:      frags,
		ContextToolDefsResolved: true,
	}
}

func capabilitySystemFrag(
	id, text string,
	kind contextfrag.Kind,
	priority int,
	requiredCapability string,
	render contextfrag.RenderPolicy,
) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:                 id,
		Kind:               kind,
		Role:               sdk.MessageRoleSystem,
		Slot:               contextfrag.SlotSystem,
		Text:               text,
		Priority:           priority,
		RetentionTier:      contextfrag.RetentionPreferred,
		RequiredCapability: requiredCapability,
		CacheClass:         contextfrag.CacheStable,
		Trust:              contextfrag.TrustSystem,
		Source:             contextfrag.SourceRunConfig,
		Collector:          sourceFragsCollectorName,
		Render:             render,
	})
}
