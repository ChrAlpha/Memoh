package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestACPRenderer_ChatModePassThroughMarkdown(t *testing.T) {
	t.Parallel()

	markdown := "# Memoh ACP Context\n\n  keep spacing exactly  \n"
	payload, rendered := renderACP(t, ACPRenderConfig{
		Mode:            ACPRenderModeChat,
		ContextMarkdown: markdown,
		ContextURI:      "memoh://custom/context",
	}, contextfrag.IntentACPRuntimePrompt)

	if payload.ContextMarkdown != markdown {
		t.Fatalf("ContextMarkdown = %q, want exact pass-through %q", payload.ContextMarkdown, markdown)
	}
	if payload.ContextURI != "memoh://custom/context" {
		t.Fatalf("ContextURI = %q, want custom URI", payload.ContextURI)
	}
	if payload.ContentHash != sha256Hex(markdown) || rendered.ContentHash != payload.ContentHash {
		t.Fatalf("ContentHash = %q outer=%q, want %q", payload.ContentHash, rendered.ContentHash, sha256Hex(markdown))
	}
}

func TestACPRenderer_ChatModeDefaultURI(t *testing.T) {
	t.Parallel()

	payload, _ := renderACP(t, ACPRenderConfig{
		Mode:            ACPRenderModeChat,
		ContextMarkdown: "context",
	}, contextfrag.IntentACPRuntimePrompt)

	if payload.ContextURI != "memoh://context/current-turn" {
		t.Fatalf("ContextURI = %q, want default current-turn URI", payload.ContextURI)
	}
}

func TestACPRenderer_ChatModeContentHashDeterministic(t *testing.T) {
	t.Parallel()

	cfg := ACPRenderConfig{Mode: ACPRenderModeChat, ContextMarkdown: "same markdown"}
	first, _ := renderACP(t, cfg, contextfrag.IntentACPRuntimePrompt)
	second, _ := renderACP(t, cfg, contextfrag.IntentACPRuntimePrompt)

	if first.ContentHash != second.ContentHash {
		t.Fatalf("ContentHash not deterministic: first=%q second=%q", first.ContentHash, second.ContentHash)
	}
	if first.ContentHash != sha256Hex(cfg.ContextMarkdown) {
		t.Fatalf("ContentHash = %q, want SHA-256 of markdown %q", first.ContentHash, sha256Hex(cfg.ContextMarkdown))
	}
}

func discussTestFrag(id, role, text string, slot contextfrag.Slot) contextfrag.ContextFrag {
	var msg sdk.Message
	switch role {
	case "assistant":
		msg = sdk.AssistantMessage(text)
	default:
		msg = sdk.UserMessage(text)
	}
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:        id,
		Message:   msg,
		Kind:      contextfrag.KindConversationEvent,
		Slot:      slot,
		Scope:     contextfrag.Scope{BotID: "bot-1"},
		Source:    "pipeline_discuss",
		Collector: "discuss_context",
	})
}

func TestACPRenderer_DiscussModeFullContextPrompt(t *testing.T) {
	t.Parallel()

	payload, _ := renderACPSelected(t, ACPRenderConfig{Mode: ACPRenderModeDiscuss}, []contextfrag.ContextFrag{
		discussTestFrag("discuss.rc.000", "user", "first message content", contextfrag.SlotHistory),
		discussTestFrag("discuss.tr.000", "assistant", "response content", contextfrag.SlotHistory),
		discussTestFrag("discuss.rc.001", "user", "default role content", contextfrag.SlotHistory),
		discussTestFrag("discuss.rc.002", "user", "   ", contextfrag.SlotHistory),
	})

	want := "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"[user]\nfirst message content\n\n" +
		"[assistant]\nresponse content\n\n" +
		"[user]\ndefault role content\n\n" +
		"Reply to the latest user-visible message when a response is appropriate."
	if payload.ContextMarkdown != want {
		t.Fatalf("ContextMarkdown mismatch:\ngot:\n%s\nwant:\n%s", payload.ContextMarkdown, want)
	}
	if payload.ContextURI != "memoh://context/discuss-turn" {
		t.Fatalf("ContextURI = %q, want discuss URI", payload.ContextURI)
	}
}

func TestACPRenderer_DiscussModeLateBinding(t *testing.T) {
	t.Parallel()

	payload, _ := renderACPSelected(t, ACPRenderConfig{Mode: ACPRenderModeDiscuss}, []contextfrag.ContextFrag{
		discussTestFrag("discuss.rc.000", "user", "question", contextfrag.SlotHistory),
		discussTestFrag("discuss.late_binding", "user", "Mentioned user should be answered.", contextfrag.SlotAfterCurrent),
	})

	want := "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"[user]\nquestion\n\n" +
		"Reply to the latest user-visible message when a response is appropriate.\n\n" +
		"Mentioned user should be answered."
	if payload.ContextMarkdown != want {
		t.Fatalf("ContextMarkdown mismatch:\ngot:\n%s\nwant:\n%s", payload.ContextMarkdown, want)
	}
}

func TestACPRenderer_DiscussModeEmptySelection(t *testing.T) {
	t.Parallel()

	payload, _ := renderACPSelected(t, ACPRenderConfig{Mode: ACPRenderModeDiscuss}, nil)

	want := "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"Reply to the latest user-visible message when a response is appropriate."
	if payload.ContextMarkdown != want {
		t.Fatalf("ContextMarkdown = %q, want header plus footer", payload.ContextMarkdown)
	}
}

func TestACPRenderer_ChatModeFragmentsWinOverLegacyMarkdown(t *testing.T) {
	t.Parallel()

	renderer := &ACPFullContextRenderer{Config: ACPRenderConfig{
		Mode:            ACPRenderModeChat,
		ContextMarkdown: "LEGACY_DOCUMENT",
	}}
	selected := []contextfrag.ContextFrag{
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:        "acp.section.000",
			Kind:      contextfrag.KindACPContext,
			Role:      sdk.MessageRoleSystem,
			Slot:      contextfrag.SlotSystem,
			Text:      "FRAGMENT_DOCUMENT",
			Scope:     contextfrag.Scope{BotID: "bot-1"},
			Source:    "acp_context",
			Collector: "acp_sections",
		}),
	}
	rendered, err := renderer.Render(context.Background(), RenderInput{
		Intent:    contextfrag.IntentACPRuntimePrompt,
		Target:    contextfrag.RenderACPFullContext,
		Selected:  selected,
		Placement: IdentityPlacer{}.Place(selected, contextfrag.IntentACPRuntimePrompt),
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	payload := rendered.Data.(*ACPRenderedPayload)
	if !strings.Contains(payload.ContextMarkdown, "FRAGMENT_DOCUMENT") {
		t.Fatalf("fragments must win, got %q", payload.ContextMarkdown)
	}
	if strings.Contains(payload.ContextMarkdown, "LEGACY_DOCUMENT") {
		t.Fatalf("legacy markdown must not leak when fragments are selected, got %q", payload.ContextMarkdown)
	}
}

func TestACPRenderer_ChatModeExcludesCurrentUserFragment(t *testing.T) {
	t.Parallel()

	selected := []contextfrag.ContextFrag{
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:        "acp.section.000",
			Kind:      contextfrag.KindACPContext,
			Role:      sdk.MessageRoleSystem,
			Slot:      contextfrag.SlotSystem,
			Text:      "FRAGMENT_DOCUMENT",
			Scope:     contextfrag.Scope{BotID: "bot-1"},
			Source:    "acp_context",
			Collector: "acp_sections",
		}),
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:        "current_user.message",
			Kind:      contextfrag.KindCurrentUserMessage,
			Role:      sdk.MessageRoleUser,
			Slot:      contextfrag.SlotCurrentUser,
			Text:      "latest question",
			Trust:     contextfrag.TrustUser,
			Scope:     contextfrag.Scope{BotID: "bot-1"},
			Source:    contextfrag.SourceRunConfig,
			Collector: "current_user",
		}),
	}
	payload, _ := renderACPSelected(t, ACPRenderConfig{Mode: ACPRenderModeChat}, selected)

	if strings.Contains(payload.ContextMarkdown, "latest question") {
		t.Fatalf("current user message must stay out of the context document, got %q", payload.ContextMarkdown)
	}
	if !strings.Contains(payload.ContextMarkdown, "FRAGMENT_DOCUMENT") {
		t.Fatalf("context sections must still render, got %q", payload.ContextMarkdown)
	}
}

func TestACPRenderer_ChatModeOnlyCurrentUserFragmentIsError(t *testing.T) {
	t.Parallel()

	selected := []contextfrag.ContextFrag{
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:        "current_user.message",
			Kind:      contextfrag.KindCurrentUserMessage,
			Role:      sdk.MessageRoleUser,
			Slot:      contextfrag.SlotCurrentUser,
			Text:      "latest question",
			Trust:     contextfrag.TrustUser,
			Scope:     contextfrag.Scope{BotID: "bot-1"},
			Source:    contextfrag.SourceRunConfig,
			Collector: "current_user",
		}),
	}
	renderer := &ACPFullContextRenderer{Config: ACPRenderConfig{Mode: ACPRenderModeChat}}
	_, err := renderer.Render(context.Background(), RenderInput{
		Intent:    contextfrag.IntentACPRuntimePrompt,
		Target:    contextfrag.RenderACPFullContext,
		Selected:  selected,
		Placement: IdentityPlacer{}.Place(selected, contextfrag.IntentACPRuntimePrompt),
	})
	if err == nil {
		t.Fatal("chat render with only the current user fragment and no legacy markdown must fail loudly")
	}
}

func TestACPRenderer_ChatModeEmptyIsError(t *testing.T) {
	t.Parallel()

	renderer := &ACPFullContextRenderer{Config: ACPRenderConfig{Mode: ACPRenderModeChat}}
	_, err := renderer.Render(context.Background(), RenderInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Target: contextfrag.RenderACPFullContext,
	})
	if err == nil {
		t.Fatal("chat render with no fragments and no legacy markdown must fail loudly")
	}
}

func renderACP(t *testing.T, cfg ACPRenderConfig, intent contextfrag.Intent) (*ACPRenderedPayload, RenderedPayload) {
	t.Helper()
	renderer := &ACPFullContextRenderer{Config: cfg}
	rendered, err := renderer.Render(context.Background(), RenderInput{Intent: intent, Target: contextfrag.RenderACPFullContext})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	payload, ok := rendered.Data.(*ACPRenderedPayload)
	if !ok {
		t.Fatalf("Data type = %T, want *ACPRenderedPayload", rendered.Data)
	}
	if rendered.Target != contextfrag.RenderACPFullContext {
		t.Fatalf("Target = %q, want %q", rendered.Target, contextfrag.RenderACPFullContext)
	}
	return payload, rendered
}

func renderACPSelected(t *testing.T, cfg ACPRenderConfig, selected []contextfrag.ContextFrag) (*ACPRenderedPayload, RenderedPayload) {
	t.Helper()
	renderer := &ACPFullContextRenderer{Config: cfg}
	rendered, err := renderer.Render(context.Background(), RenderInput{
		Intent:    contextfrag.IntentACPRuntimePrompt,
		Target:    contextfrag.RenderACPFullContext,
		Selected:  selected,
		Placement: IdentityPlacer{}.Place(selected, contextfrag.IntentACPRuntimePrompt),
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	payload, ok := rendered.Data.(*ACPRenderedPayload)
	if !ok {
		t.Fatalf("Data type = %T, want *ACPRenderedPayload", rendered.Data)
	}
	return payload, rendered
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
