package contextview

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestEquivalence_BasicSystemMessagesAndQuery(t *testing.T) {
	t.Parallel()

	assertContextViewEquivalent(t, legacyInput{
		system:   "You are helpful.",
		messages: []sdk.Message{sdk.UserMessage("hello"), sdk.AssistantMessage("hi")},
		query:    "What is 2+2?",
	})
}

func TestEquivalence_SystemWithToolUsage(t *testing.T) {
	t.Parallel()

	toolUsage := "## Tool usage\nUse tools carefully."
	assertContextViewEquivalent(t, legacyInput{
		system:    "Base system prompt.\n\n" + toolUsage + "\n\nTail guidance.",
		toolUsage: toolUsage,
		messages:  []sdk.Message{sdk.UserMessage("hello")},
		query:     "Continue.",
	})
}

func TestEquivalence_SystemWithWorkspaceInstruction(t *testing.T) {
	t.Parallel()

	toolUsage := "## Tool usage\nUse tools carefully."
	assertContextViewEquivalent(t, legacyInput{
		system:    "Base system prompt.\n\n" + toolUsage + "\n\n## Workspace instruction files\n- AGENTS.md",
		toolUsage: toolUsage,
		messages:  []sdk.Message{sdk.UserMessage("hello")},
		query:     "Continue.",
	})
}

func TestEquivalence_NoQuery(t *testing.T) {
	t.Parallel()

	assertContextViewEquivalent(t, legacyInput{
		system:   "You are helpful.",
		messages: []sdk.Message{sdk.UserMessage("hello")},
	})
}

func TestEquivalence_InlineImages(t *testing.T) {
	t.Parallel()

	assertContextViewEquivalent(t, legacyInput{
		system: "You can inspect images.",
		query:  "What is in this image?",
		images: []sdk.ImagePart{
			{Image: "data:image/png;base64,abc", MediaType: "image/png"},
			{Image: "", MediaType: "image/png"},
			{Image: "data:image/jpeg;base64,def", MediaType: "image/jpeg"},
		},
	})
}

func TestEquivalence_ToolMessage(t *testing.T) {
	t.Parallel()

	assertContextViewEquivalent(t, legacyInput{
		system: "You are helpful.",
		messages: []sdk.Message{
			sdk.AssistantMessage("I will search."),
			sdk.ToolMessage(sdk.ToolResultPart{ToolCallID: "call-1", ToolName: "search", Result: map[string]any{"ok": true}}),
		},
		query: "Summarize result.",
	})
}

func TestEquivalence_EmptyInput(t *testing.T) {
	t.Parallel()

	assertContextViewEquivalent(t, legacyInput{})
}

type legacyInput struct {
	system    string
	messages  []sdk.Message
	query     string
	images    []sdk.ImagePart
	toolUsage string
}

func assertContextViewEquivalent(t *testing.T, input legacyInput) {
	t.Helper()
	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1", TurnID: "t1"}
	legacy := contextfrag.Compile(contextfrag.CompileInput{
		Source:       contextfrag.SourceRunConfig,
		Scope:        scope,
		System:       input.system,
		Messages:     input.messages,
		Query:        input.query,
		InlineImages: input.images,
		ToolUsage:    input.toolUsage,
	})

	builder := NewBuilder(
		NewMapCollectorRegistry(
			&SystemPromptCollector{},
			&HistoryMessagesCollector{},
			&CurrentUserCollector{},
			&InlineImageCollector{},
		),
		PassthroughSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(&SDKMessagesRenderer{}),
	)
	view, err := builder.Build(context.Background(), BuildInput{
		Scope:  scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{
			{Name: "system_prompt", Config: SystemPromptConfig{System: input.system, ToolUsage: input.toolUsage}},
			{Name: "history_messages", Config: HistoryMessagesConfig{Messages: input.messages}},
			{Name: "current_user", Config: CurrentUserConfig{Query: input.query}},
			{Name: "inline_images", Config: InlineImageConfig{Images: input.images}},
		},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	rendered, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		t.Fatalf("rendered data type = %T, want *SDKRenderedPayload", view.Rendered[contextfrag.RenderSDKMessages].Data)
	}

	if legacy.System != rendered.System {
		t.Fatalf("System = %q, want %q", rendered.System, legacy.System)
	}
	if legacy.Query != rendered.Query {
		t.Fatalf("Query = %q, want %q", rendered.Query, legacy.Query)
	}
	assertMessagesEqual(t, rendered.Messages, legacy.Messages)
	assertImagesEqual(t, rendered.InlineImages, legacy.InlineImages)
}

func assertMessagesEqual(t *testing.T, got []sdk.Message, want []sdk.Message) {
	t.Helper()
	gotJSON := marshalMessages(t, got)
	wantJSON := marshalMessages(t, want)
	if gotJSON != wantJSON {
		t.Fatalf("messages JSON = %s, want %s", gotJSON, wantJSON)
	}
}

func assertImagesEqual(t *testing.T, got []sdk.ImagePart, want []sdk.ImagePart) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("images = %#v, want %#v", got, want)
	}
}

func marshalMessages(t *testing.T, messages []sdk.Message) string {
	t.Helper()
	data, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	return string(data)
}
