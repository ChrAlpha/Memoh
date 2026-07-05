package contextview

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestSystemPromptCollector_SplitsToolUsage(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	toolUsage := "## Tool usage\nUse tools carefully."
	system := "Base system prompt.\n\n" + toolUsage + "\n\nTail guidance."

	frags := collectSystemPrompt(t, scope, system, toolUsage)

	if len(frags) != 3 {
		t.Fatalf("frags = %d, want 3", len(frags))
	}
	assertTextFrag(t, frags[0], "system.prompt", contextfrag.KindSystemPrompt, "Base system prompt.", 20, contextfrag.SourceRunConfig)
	assertTextFrag(t, frags[1], "system.tool_usage", contextfrag.KindToolUsage, toolUsage, 45, contextfrag.SourceAgentToolUsage)
	assertTextFrag(t, frags[2], "system.prompt.tail", contextfrag.KindSystemPrompt, "Tail guidance.", 50, contextfrag.SourceRunConfig)
}

func TestSystemPromptCollector_WorkspaceInstruction(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	toolUsage := "## Tool usage\nUse tools carefully."
	workspace := "## Workspace instruction files\n- AGENTS.md"
	system := "Base system prompt.\n\n" + toolUsage + "\n\n" + workspace

	frags := collectSystemPrompt(t, scope, system, toolUsage)

	if len(frags) != 3 {
		t.Fatalf("frags = %d, want 3", len(frags))
	}
	assertTextFrag(t, frags[2], "system.workspace_instructions", contextfrag.KindWorkspaceInstruction, workspace, 50, contextfrag.SourceRunConfig)
}

func TestSystemPromptCollector_EmptySystem(t *testing.T) {
	t.Parallel()

	frags := collectSystemPrompt(t, contextfrag.Scope{BotID: "bot-1"}, "  ", "tool usage")
	if frags != nil {
		t.Fatalf("frags = %#v, want nil", frags)
	}
}

func TestSystemPromptCollector_NoToolUsage(t *testing.T) {
	t.Parallel()

	frags := collectSystemPrompt(t, contextfrag.Scope{BotID: "bot-1"}, "  Base system prompt.  ", "")

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	assertTextFrag(t, frags[0], "system.prompt", contextfrag.KindSystemPrompt, "Base system prompt.", 20, contextfrag.SourceRunConfig)
}

func TestSystemPromptCollector_SplitWorkspaceUsesSharedAnchor(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	system := "Base system prompt." + contextfrag.WorkspaceInstructionAnchor + "\n\n- AGENTS.md"

	collector := &SystemPromptCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: SystemPromptConfig{System: system, SplitWorkspace: true},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(frags) != 2 {
		t.Fatalf("frags = %d, want 2", len(frags))
	}
	assertTextFrag(t, frags[0], "system.prompt", contextfrag.KindSystemPrompt, "Base system prompt.", 20, contextfrag.SourceRunConfig)
	assertTextFrag(t, frags[1], "system.workspace_instructions", contextfrag.KindWorkspaceInstruction, "## Workspace instruction files\n\n- AGENTS.md", 50, contextfrag.SourceRunConfig)
}

func TestHistoryMessagesCollector_MultipleMessages(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	messages := []sdk.Message{
		sdk.SystemMessage("system policy"),
		sdk.UserMessage("hello"),
		sdk.AssistantMessage("hi"),
	}

	frags := collectHistoryMessages(t, scope, messages)

	if len(frags) != len(messages) {
		t.Fatalf("frags = %d, want %d", len(frags), len(messages))
	}
	assertMessageFrag(t, frags[0], "message.000", contextfrag.KindSystemPolicy, contextfrag.CacheDynamic, contextfrag.TrustSystem, 30, sdk.MessageRoleSystem)
	assertMessageFrag(t, frags[1], "message.001", contextfrag.KindConversationEvent, contextfrag.CacheNever, contextfrag.TrustExternal, 70, sdk.MessageRoleUser)
	assertMessageFrag(t, frags[2], "message.002", contextfrag.KindConversationEvent, contextfrag.CacheNever, contextfrag.TrustWorkspace, 70, sdk.MessageRoleAssistant)
}

func TestHistoryMessagesCollector_EmptyMessages(t *testing.T) {
	t.Parallel()

	frags := collectHistoryMessages(t, contextfrag.Scope{BotID: "bot-1"}, nil)
	if frags != nil {
		t.Fatalf("frags = %#v, want nil", frags)
	}
}

func TestHistoryMessagesCollector_ToolMessage(t *testing.T) {
	t.Parallel()

	frags := collectHistoryMessages(t, contextfrag.Scope{BotID: "bot-1"}, []sdk.Message{
		sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "call-1", ToolName: "search", Result: "done"}),
	})

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	assertMessageFrag(t, frags[0], "message.000", contextfrag.KindConversationEvent, contextfrag.CacheNever, contextfrag.TrustWorkspace, 55, sdk.MessageRoleTool)
}

func TestCurrentUserCollector_NonEmpty(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	collector := &CurrentUserCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: CurrentUserConfig{Query: "  What is 2+2?  "},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	frag := frags[0]
	assertTextFrag(t, frag, "current_user.message", contextfrag.KindCurrentUserMessage, "What is 2+2?", 90, contextfrag.SourceRunConfig)
	if frag.Slot != contextfrag.SlotCurrentUser {
		t.Fatalf("Slot = %q, want %q", frag.Slot, contextfrag.SlotCurrentUser)
	}
	if frag.CacheClass != contextfrag.CacheNever {
		t.Fatalf("CacheClass = %q, want %q", frag.CacheClass, contextfrag.CacheNever)
	}
	if frag.Trust != contextfrag.TrustUser {
		t.Fatalf("Trust = %q, want %q", frag.Trust, contextfrag.TrustUser)
	}
}

func TestCurrentUserCollector_Empty(t *testing.T) {
	t.Parallel()

	collector := &CurrentUserCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: CurrentUserConfig{Query: "  "},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if frags != nil {
		t.Fatalf("frags = %#v, want nil", frags)
	}
}

func TestInlineImageCollector_FiltersEmptyImages(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	collector := &InlineImageCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: InlineImageConfig{Images: []sdk.ImagePart{
			{Image: "", MediaType: "image/png"},
			{Image: "data:image/png;base64,abc", MediaType: "image/png"},
		}},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	frag := frags[0]
	if frag.ID != "current_user.images" {
		t.Fatalf("ID = %q, want current_user.images", frag.ID)
	}
	if frag.Kind != contextfrag.KindNativeImage {
		t.Fatalf("Kind = %q, want %q", frag.Kind, contextfrag.KindNativeImage)
	}
	if len(frag.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(frag.Parts))
	}
	if frag.Parts[0].SDKImage == nil || frag.Parts[0].SDKImage.Image != "data:image/png;base64,abc" {
		t.Fatalf("image part not preserved: %#v", frag.Parts[0])
	}
}

func TestInlineImageCollector_NoImages(t *testing.T) {
	t.Parallel()

	collector := &InlineImageCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: InlineImageConfig{Images: []sdk.ImagePart{{Image: "  ", MediaType: "image/png"}}},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if frags != nil {
		t.Fatalf("frags = %#v, want nil", frags)
	}
}

func collectSystemPrompt(t *testing.T, scope contextfrag.Scope, system string, toolUsage string) []contextfrag.ContextFrag {
	t.Helper()
	collector := &SystemPromptCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: SystemPromptConfig{System: system, ToolUsage: toolUsage},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return frags
}

func collectHistoryMessages(t *testing.T, scope contextfrag.Scope, messages []sdk.Message) []contextfrag.ContextFrag {
	t.Helper()
	collector := &HistoryMessagesCollector{}
	frags, err := collector.Collect(context.Background(), CollectRequest{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: HistoryMessagesConfig{Messages: messages},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	return frags
}

func assertTextFrag(t *testing.T, frag contextfrag.ContextFrag, id string, kind contextfrag.Kind, text string, priority int, source string) {
	t.Helper()
	if frag.ID != id {
		t.Fatalf("ID = %q, want %q", frag.ID, id)
	}
	if frag.Kind != kind {
		t.Fatalf("Kind = %q, want %q", frag.Kind, kind)
	}
	if frag.Slot != contextfrag.SlotSystem && id != "current_user.message" {
		t.Fatalf("Slot = %q, want %q", frag.Slot, contextfrag.SlotSystem)
	}
	if frag.Priority != priority {
		t.Fatalf("Priority = %d, want %d", frag.Priority, priority)
	}
	if frag.Provenance.Source != source {
		t.Fatalf("Source = %q, want %q", frag.Provenance.Source, source)
	}
	if frag.Render.Format == "" {
		t.Fatalf("Render.Format should be set")
	}
	if len(frag.Parts) != 1 || frag.Parts[0].Type != contextfrag.PartText || frag.Parts[0].Text != text {
		t.Fatalf("text part = %#v, want %q", frag.Parts, text)
	}
}

func assertMessageFrag(t *testing.T, frag contextfrag.ContextFrag, id string, kind contextfrag.Kind, cache contextfrag.CacheClass, trust contextfrag.TrustLevel, priority int, role sdk.MessageRole) {
	t.Helper()
	if frag.ID != id {
		t.Fatalf("ID = %q, want %q", frag.ID, id)
	}
	if frag.Kind != kind {
		t.Fatalf("Kind = %q, want %q", frag.Kind, kind)
	}
	if frag.Slot != contextfrag.SlotHistory {
		t.Fatalf("Slot = %q, want %q", frag.Slot, contextfrag.SlotHistory)
	}
	if frag.CacheClass != cache {
		t.Fatalf("CacheClass = %q, want %q", frag.CacheClass, cache)
	}
	if frag.Trust != trust {
		t.Fatalf("Trust = %q, want %q", frag.Trust, trust)
	}
	if frag.Priority != priority {
		t.Fatalf("Priority = %d, want %d", frag.Priority, priority)
	}
	if frag.Role != role {
		t.Fatalf("Role = %q, want %q", frag.Role, role)
	}
	if len(frag.Parts) != 1 || frag.Parts[0].Type != contextfrag.PartSDKMessage || frag.Parts[0].SDKMessage == nil {
		t.Fatalf("message part = %#v, want SDK message", frag.Parts)
	}
}
