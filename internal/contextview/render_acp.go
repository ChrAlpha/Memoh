package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
)

const (
	defaultACPContextURI        = "memoh://context/current-turn"
	defaultDiscussACPContextURI = "memoh://context/discuss-turn"
)

type ACPRenderedPayload struct {
	ContextMarkdown string
	ContextURI      string
	ContentHash     string
}

type ACPRenderMode string

const (
	ACPRenderModeChat    ACPRenderMode = "chat"
	ACPRenderModeDiscuss ACPRenderMode = "discuss"
)

type ACPRenderConfig struct {
	Mode               ACPRenderMode
	ContextMarkdown    string
	ContextURI         string
	DiscussMessages    []pipeline.ContextMessage
	DiscussLateBinding string
}

type ACPFullContextRenderer struct {
	Config ACPRenderConfig
}

func (*ACPFullContextRenderer) Target() contextfrag.RenderTarget {
	return contextfrag.RenderACPFullContext
}

func (r *ACPFullContextRenderer) Render(_ context.Context, input RenderInput) (RenderedPayload, error) {
	cfg := r.Config
	markdown, uri := cfg.ContextMarkdown, cfg.ContextURI

	if acpRenderMode(cfg) == ACPRenderModeDiscuss {
		markdown = buildDiscussACPFullContextPrompt(cfg.DiscussMessages, cfg.DiscussLateBinding)
		uri = defaultDiscussACPContextURI
	} else {
		if len(input.Selected) > 0 {
			rendered, err := renderACPSelectedMarkdown(input)
			if err != nil {
				return RenderedPayload{}, err
			}
			markdown = rendered
		}
		if uri == "" {
			uri = defaultACPContextURI
		}
	}

	hash := textContentHash(markdown)
	payload := &ACPRenderedPayload{
		ContextMarkdown: markdown,
		ContextURI:      uri,
		ContentHash:     hash,
	}
	return RenderedPayload{
		Target:      contextfrag.RenderACPFullContext,
		ContentHash: hash,
		Data:        payload,
	}, nil
}

func renderACPSelectedMarkdown(input RenderInput) (string, error) {
	ordered, err := orderedSelectedFrags(input.Selected, input.Placement)
	if err != nil {
		return "", err
	}
	blocks := make([]string, 0, len(ordered))
	for _, frag := range ordered {
		for _, part := range frag.Parts {
			if part.Type != contextfrag.PartText {
				continue
			}
			if text := strings.TrimSpace(part.Text); text != "" {
				blocks = append(blocks, text)
			}
		}
	}
	return FinalizeACPContextMarkdown(blocks), nil
}

func acpRenderMode(cfg ACPRenderConfig) ACPRenderMode {
	if cfg.Mode != "" {
		return cfg.Mode
	}
	return ACPRenderModeChat
}

func buildDiscussACPFullContextPrompt(messages []pipeline.ContextMessage, lateBinding string) string {
	var b strings.Builder
	b.WriteString("You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n")
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		b.WriteString("[")
		b.WriteString(role)
		b.WriteString("]\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	b.WriteString("Reply to the latest user-visible message when a response is appropriate.")
	if strings.TrimSpace(lateBinding) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(lateBinding))
	}
	return strings.TrimSpace(b.String())
}

func textContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
