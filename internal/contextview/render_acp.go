package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
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
	Mode            ACPRenderMode
	ContextMarkdown string
	ContextURI      string
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
		rendered, err := renderDiscussACPSelectedPrompt(input)
		if err != nil {
			return RenderedPayload{}, err
		}
		markdown = rendered
		uri = defaultDiscussACPContextURI
	} else {
		switch {
		case hasNonCurrentUserSelection(input.Selected):
			rendered, err := renderACPSelectedMarkdown(input)
			if err != nil {
				return RenderedPayload{}, err
			}
			markdown = rendered
		case strings.TrimSpace(markdown) == "":
			// Chat mode must never silently produce an empty context
			// document: either context fragments were selected (the current
			// user message alone does not count — it is delivered as the
			// prompt, never as context) or the caller explicitly provided a
			// legacy markdown document.
			return RenderedPayload{}, errors.New("acp chat render: no selected context fragments and no legacy context markdown")
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

// hasNonCurrentUserSelection reports whether the selection contains anything
// beyond the current user message. The current-user fragment is always
// selected (it becomes the ACP prompt), so it must not mask an otherwise
// empty context document.
func hasNonCurrentUserSelection(selected []contextfrag.ContextFrag) bool {
	for _, frag := range selected {
		if frag.Slot != contextfrag.SlotCurrentUser {
			return true
		}
	}
	return false
}

// renderACPSelectedMarkdown assembles the ACP context resource document.
// The current user message is selected and manifest-recorded with the view,
// but it is delivered as the ACP prompt itself, never as part of the context
// document, so SlotCurrentUser fragments are skipped here.
func renderACPSelectedMarkdown(input RenderInput) (string, error) {
	ordered, err := orderedSelectedFrags(input.Selected, input.Placement)
	if err != nil {
		return "", err
	}
	blocks := make([]string, 0, len(ordered))
	for _, frag := range ordered {
		if frag.Slot == contextfrag.SlotCurrentUser {
			continue
		}
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

// renderDiscussACPSelectedPrompt renders the discuss-mode full-context prompt
// from the selected fragments: conversation fragments become "[role]" blocks
// in placement order and the late-binding fragment (SlotAfterCurrent) lands
// after the closing instruction instead of inside the conversation.
func renderDiscussACPSelectedPrompt(input RenderInput) (string, error) {
	ordered, err := orderedSelectedFrags(input.Selected, input.Placement)
	if err != nil {
		return "", err
	}
	var lateBindings []string
	var b strings.Builder
	b.WriteString("You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n")
	for _, frag := range ordered {
		role, content := discussACPBlock(frag)
		if content == "" {
			continue
		}
		if frag.Slot == contextfrag.SlotAfterCurrent {
			lateBindings = append(lateBindings, content)
			continue
		}
		b.WriteString("[")
		b.WriteString(role)
		b.WriteString("]\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	b.WriteString("Reply to the latest user-visible message when a response is appropriate.")
	for _, lateBinding := range lateBindings {
		b.WriteString("\n\n")
		b.WriteString(lateBinding)
	}
	return strings.TrimSpace(b.String()), nil
}

func discussACPBlock(frag contextfrag.ContextFrag) (string, string) {
	role := strings.TrimSpace(string(frag.Role))
	if msg := discussFragMessage(frag); msg != nil {
		if msgRole := strings.TrimSpace(string(msg.Role)); msgRole != "" {
			role = msgRole
		}
		var texts []string
		for _, part := range msg.Content {
			if text, ok := part.(sdk.TextPart); ok {
				if trimmed := strings.TrimSpace(text.Text); trimmed != "" {
					texts = append(texts, trimmed)
				}
			}
		}
		return roleOrUser(role), strings.Join(texts, "\n")
	}
	var texts []string
	for _, part := range frag.Parts {
		if part.Type != contextfrag.PartText {
			continue
		}
		if text := strings.TrimSpace(part.Text); text != "" {
			texts = append(texts, text)
		}
	}
	return roleOrUser(role), strings.Join(texts, "\n")
}

func roleOrUser(role string) string {
	if role == "" {
		return "user"
	}
	return role
}

func textContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
