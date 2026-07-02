package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
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

func TestACPRenderer_DiscussModeFullContextPrompt(t *testing.T) {
	t.Parallel()

	payload, _ := renderACP(t, ACPRenderConfig{
		Mode: ACPRenderModeDiscuss,
		DiscussMessages: []pipeline.ContextMessage{
			{Role: "user", Content: "first message content"},
			{Role: "assistant", Content: "response content"},
			{Role: "", Content: "default role content"},
			{Role: "tool", Content: "   "},
		},
	}, contextfrag.IntentDiscussReply)

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

	payload, _ := renderACP(t, ACPRenderConfig{
		Mode: ACPRenderModeDiscuss,
		DiscussMessages: []pipeline.ContextMessage{
			{Role: "user", Content: "question"},
		},
		DiscussLateBinding: "  Mentioned user should be answered.  ",
	}, contextfrag.IntentDiscussReply)

	want := "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"[user]\nquestion\n\n" +
		"Reply to the latest user-visible message when a response is appropriate.\n\n" +
		"Mentioned user should be answered."
	if payload.ContextMarkdown != want {
		t.Fatalf("ContextMarkdown mismatch:\ngot:\n%s\nwant:\n%s", payload.ContextMarkdown, want)
	}
}

func TestACPRenderer_DiscussModeEmptyMessages(t *testing.T) {
	t.Parallel()

	payload, _ := renderACP(t, ACPRenderConfig{
		Mode: ACPRenderModeDiscuss,
	}, contextfrag.IntentDiscussReply)

	want := "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"Reply to the latest user-visible message when a response is appropriate."
	if payload.ContextMarkdown != want {
		t.Fatalf("ContextMarkdown = %q, want header plus footer", payload.ContextMarkdown)
	}
}

func TestACPRenderer_EmptyConfig(t *testing.T) {
	t.Parallel()

	payload, _ := renderACP(t, ACPRenderConfig{}, contextfrag.IntentACPRuntimePrompt)

	if payload.ContextMarkdown != "" {
		t.Fatalf("ContextMarkdown = %q, want empty", payload.ContextMarkdown)
	}
	if payload.ContextURI != "memoh://context/current-turn" {
		t.Fatalf("ContextURI = %q, want default current-turn URI", payload.ContextURI)
	}
	if payload.ContentHash != sha256Hex("") {
		t.Fatalf("ContentHash = %q, want SHA-256 of empty markdown", payload.ContentHash)
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

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
