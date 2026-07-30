package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func systemTextFrag(id, text string, kind contextfrag.Kind, priority int) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:         id,
		Kind:       kind,
		Role:       sdk.MessageRoleSystem,
		Slot:       contextfrag.SlotSystem,
		Text:       text,
		Priority:   priority,
		CacheClass: contextfrag.CacheStable,
		Trust:      contextfrag.TrustSystem,
		Scope:      contextfrag.Scope{BotID: "bot-1"},
		Source:     contextfrag.SourceRunConfig,
		Collector:  "system_prompt",
	})
}

func fragsFirstFixture() agentpkg.RunConfig {
	memoryMsg := sdk.UserMessage("remembered fact")
	return agentpkg.RunConfig{
		System:   "LEGACY_SYSTEM_IGNORED",
		Messages: []sdk.Message{sdk.UserMessage("LEGACY_MESSAGE_IGNORED")},
		ContextSourceFrags: []contextfrag.ContextFrag{
			systemTextFrag("system.prompt", "base system", contextfrag.KindSystemPrompt, 20),
			systemTextFrag("system.workspace_instructions", "## Workspace instruction files\n\nworkspace text", contextfrag.KindWorkspaceInstruction, 50),
			historyMessageFrag("message.000", sdk.UserMessage("history question")),
			contextfrag.MessageFrag(contextfrag.MessageFragInput{
				ID:        "memory.recall",
				Message:   memoryMsg,
				Kind:      contextfrag.KindMemoryRecall,
				Slot:      contextfrag.SlotHistory,
				Scope:     contextfrag.Scope{BotID: "bot-1"},
				Source:    "memory_context",
				Collector: "memory_context",
				Budget:    contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
			}),
			contextfrag.TextFrag(contextfrag.TextFragInput{
				ID:         "current_user.message",
				Kind:       contextfrag.KindCurrentUserMessage,
				Role:       sdk.MessageRoleUser,
				Slot:       contextfrag.SlotCurrentUser,
				Text:       "current question",
				Priority:   90,
				CacheClass: contextfrag.CacheNever,
				Trust:      contextfrag.TrustUser,
				Scope:      contextfrag.Scope{BotID: "bot-1"},
				Source:     contextfrag.SourceRunConfig,
				Collector:  "current_user",
			}),
		},
		ContextToolUsage: "## Tool usage\n\nUSE_TOOLS\n\nUNICODE 用法",
		ContextToolUsageFrags: []contextfrag.ContextFrag{
			toolUsageTestFrag("system.tool_usage.header", "## Tool usage", "zeta_tool", 0),
			toolUsageTestFrag("system.tool_usage.zeta_tool", "USE_TOOLS", "zeta_tool", 1),
			toolUsageTestFrag("system.tool_usage.alpha_tool", "UNICODE 用法", "alpha_tool", 2),
		},
		ContextToolDefs: []contextfrag.ToolDefAccounting{
			{Name: "zeta_tool"},
			{Name: "alpha_tool"},
		},
		ContextScope: contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
	}
}

func toolUsageTestFrag(id, text, capability string, index int) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:                 id,
		Kind:               contextfrag.KindToolUsage,
		Role:               sdk.MessageRoleSystem,
		Slot:               contextfrag.SlotSystem,
		Text:               text,
		Priority:           45,
		RetentionTier:      contextfrag.RetentionPreferred,
		RequiredCapability: capability,
		CacheClass:         contextfrag.CacheStable,
		Trust:              contextfrag.TrustSystem,
		Scope:              contextfrag.Scope{BotID: "bot-1"},
		Source:             contextfrag.SourceAgentToolUsage,
		Collector:          "assemble_tools",
		Index:              index,
		Render: contextfrag.RenderPolicy{
			Format:      contextfrag.RenderMarkdown,
			GroupID:     "system.tool_usage",
			GroupJoiner: "\n\n",
		},
	})
}

func TestApplyProviderRunConfigFragsFirst(t *testing.T) {
	t.Parallel()

	got := applyProviderRunConfigOK(context.Background(), nil, fragsFirstFixture())

	wantSystem := "base system\n\n## Tool usage\n\nUSE_TOOLS\n\nUNICODE 用法\n\n## Workspace instruction files\n\nworkspace text"
	if got.System != wantSystem {
		t.Fatalf("system = %q, want tool usage between prompt and workspace:\n%q", got.System, wantSystem)
	}
	if strings.Contains(got.System, "LEGACY_SYSTEM_IGNORED") {
		t.Fatal("legacy system field must not leak into the fragment-first render")
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want history + memory + query", len(got.Messages))
	}
	for _, msg := range got.Messages {
		if text, ok := msg.Content[0].(sdk.TextPart); ok && strings.Contains(text.Text, "LEGACY_MESSAGE_IGNORED") {
			t.Fatal("legacy messages must not leak into the fragment-first render")
		}
	}
	last, ok := got.Messages[2].Content[0].(sdk.TextPart)
	if !ok || last.Text != "current question" {
		t.Fatalf("last message = %#v, want the current question", got.Messages[2].Content)
	}
	var toolUsageIDs []string
	for _, frag := range got.ContextFrags {
		if frag.Kind == contextfrag.KindToolUsage {
			toolUsageIDs = append(toolUsageIDs, frag.ID)
		}
	}
	wantToolUsageIDs := []string{
		"system.tool_usage.header",
		"system.tool_usage.zeta_tool",
		"system.tool_usage.alpha_tool",
	}
	if len(toolUsageIDs) != len(wantToolUsageIDs) {
		t.Fatalf("tool usage IDs = %v, want %v", toolUsageIDs, wantToolUsageIDs)
	}
	for i := range wantToolUsageIDs {
		if toolUsageIDs[i] != wantToolUsageIDs[i] {
			t.Fatalf("tool usage IDs = %v, want %v", toolUsageIDs, wantToolUsageIDs)
		}
	}
	if len(got.ContextManifest.Items) == 0 || got.ContextManifest.CachePlan == nil {
		t.Fatal("fragment-first run must produce the full lifecycle manifest")
	}
}

func TestCollectProviderSourceFragsSplitsWorkspace(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:            "base system\n\n## Workspace instruction files\n\nworkspace text",
		Messages:          []sdk.Message{sdk.UserMessage("history")},
		Query:             "current question",
		ContextMemoryText: "remembered fact",
		ContextScope:      contextfrag.Scope{BotID: "bot-1"},
	}
	frags := CollectProviderSourceFrags(context.Background(), cfg)

	ids := make([]string, 0, len(frags))
	for _, frag := range frags {
		ids = append(ids, frag.ID)
	}
	want := []string{"system.prompt", "system.workspace_instructions", "message.000", "memory.recall", "current_user.message"}
	if len(ids) != len(want) {
		t.Fatalf("frag ids = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("frag ids = %v, want %v", ids, want)
		}
	}
	if frags[1].Kind != contextfrag.KindWorkspaceInstruction || frags[1].Priority != 50 {
		t.Fatalf("workspace frag metadata = %s/%d, want workspace_instruction/50", frags[1].Kind, frags[1].Priority)
	}
}

func TestCollectNonSystemProviderSourceFragsExcludesSystemFrags(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:            "base system\n\n## Workspace instruction files\n\nworkspace text",
		Messages:          []sdk.Message{sdk.UserMessage("history")},
		Query:             "current question",
		ContextMemoryText: "remembered fact",
		ContextHookText:   "hook note",
		InlineImages:      []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
		ContextScope:      contextfrag.Scope{BotID: "bot-1"},
	}

	frags := CollectNonSystemProviderSourceFrags(context.Background(), cfg)

	for _, frag := range frags {
		switch frag.Kind {
		case contextfrag.KindSystemPrompt, contextfrag.KindWorkspaceInstruction, contextfrag.KindBotIdentity, contextfrag.KindPlatformIdentity:
			t.Fatalf("non-system collection must not include system-derived Kind %s: %#v", frag.Kind, frag)
		}
	}

	ids := make([]string, 0, len(frags))
	for _, frag := range frags {
		ids = append(ids, frag.ID)
	}
	want := []string{"message.000", "memory.recall", "hook_context.message", "current_user.message", "current_user.images"}
	if len(ids) != len(want) {
		t.Fatalf("frag ids = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("frag ids = %v, want %v", ids, want)
		}
	}
}
