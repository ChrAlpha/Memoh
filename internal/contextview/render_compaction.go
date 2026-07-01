package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

const compactionSystemPrompt = `You are a conversation summarizer. Given a conversation history, produce a concise summary that preserves:
- Key facts, decisions, and agreements
- User preferences and requests
- Important context needed for continuing the conversation
- Names, dates, numbers, and specific details
- Tool usage outcomes and their results

If <prior_context> is provided, it contains summaries of earlier conversation segments. Use them ONLY to understand the conversation flow and maintain continuity. Do NOT include, repeat, or rephrase any content from <prior_context> in your output.

For tool results, only include key outcomes; ignore intermediate steps or errors.

Output ONLY the summary of the new conversation segment. No preamble, no headers.`

type CompactionRenderedPayload struct {
	SystemPrompt  string
	UserPrompt    string
	CandidateRefs []contextfrag.ContextRef
}

type CompactionPromptRenderer struct {
	PriorSummaries []string
}

type compactionMessageEntry struct {
	Role    string
	Content string
}

func (*CompactionPromptRenderer) Target() contextfrag.RenderTarget {
	return contextfrag.RenderCompactionPrompt
}

func (r *CompactionPromptRenderer) Render(_ context.Context, input RenderInput) (RenderedPayload, error) {
	ordered, err := orderedSelectedFrags(input.Selected, input.Placement)
	if err != nil {
		return RenderedPayload{}, err
	}

	entries := make([]compactionMessageEntry, 0, len(ordered))
	refs := make([]contextfrag.ContextRef, 0, len(ordered))
	for _, frag := range ordered {
		ref := frag.Ref
		if err := contextfrag.ValidateContextRef(ref); err != nil {
			ref = contextfrag.WithContextRef(frag, ref).Ref
		}
		refs = append(refs, ref)

		content := strings.TrimSpace(renderCompactionFragContent(frag))
		if content == "" {
			continue
		}
		entries = append(entries, compactionMessageEntry{
			Role:    string(compactionFragRole(frag)),
			Content: content,
		})
	}

	payload := &CompactionRenderedPayload{
		SystemPrompt:  compactionSystemPrompt,
		UserPrompt:    buildCompactionUserPrompt(r.PriorSummaries, entries),
		CandidateRefs: refs,
	}
	hash, err := compactionRenderedPayloadHash(payload)
	if err != nil {
		return RenderedPayload{}, err
	}
	return RenderedPayload{
		Target:      contextfrag.RenderCompactionPrompt,
		ContentHash: hash,
		Data:        payload,
	}, nil
}

func compactionFragRole(frag contextfrag.ContextFrag) sdk.MessageRole {
	if frag.Role != "" {
		return frag.Role
	}
	for _, part := range frag.Parts {
		if msg := sdkMessagePart(part); msg != nil && msg.Role != "" {
			return msg.Role
		}
	}
	return sdk.MessageRoleAssistant
}

func renderCompactionFragContent(frag contextfrag.ContextFrag) string {
	segments := make([]string, 0, len(frag.Parts))
	for _, part := range frag.Parts {
		switch part.Type {
		case contextfrag.PartText:
			if text := strings.TrimSpace(part.Text); text != "" {
				segments = append(segments, text)
			}
		case contextfrag.PartImage:
			segments = append(segments, "[image]")
		case contextfrag.PartSDKMessage:
			if msg := sdkMessagePart(part); msg != nil {
				segments = append(segments, renderCompactionMessageContent(*msg)...)
			}
		}
	}
	return strings.Join(segments, "\n")
}

func renderCompactionMessageContent(msg sdk.Message) []string {
	segments := make([]string, 0, len(msg.Content))
	for _, part := range msg.Content {
		switch p := part.(type) {
		case sdk.TextPart:
			if text := strings.TrimSpace(p.Text); text != "" {
				segments = append(segments, text)
			}
		case sdk.ImagePart:
			segments = append(segments, "[image]")
		case sdk.FilePart:
			segments = append(segments, "[file]")
		case sdk.ToolCallPart:
			segments = append(segments, compactionToolCallMarker(p.ToolName))
		case sdk.ToolResultPart:
			segments = append(segments, compactionToolResultText(p.Result))
		}
	}
	return segments
}

func compactionToolCallMarker(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "[tool_call]"
	}
	return "[tool_call: " + name + "]"
}

func compactionToolResultText(value any) string {
	switch v := value.(type) {
	case nil:
		return "[tool result]"
	case string:
		if text := strings.TrimSpace(v); text != "" {
			return text
		}
	default:
		raw, err := json.Marshal(v)
		if err == nil && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "null" {
			return string(raw)
		}
	}
	return "[tool result]"
}

func buildCompactionUserPrompt(priorSummaries []string, messages []compactionMessageEntry) string {
	var sb strings.Builder
	if len(priorSummaries) > 0 {
		sb.WriteString("<prior_context>\n")
		sb.WriteString("The following are summaries of earlier parts of this conversation. They are provided ONLY as reference context to help you understand the conversation flow. Do NOT include or repeat any of this content in your output summary.\n\n")
		sb.WriteString(strings.Join(priorSummaries, "\n---\n"))
		sb.WriteString("\n</prior_context>\n\n")
		sb.WriteString("Now summarize the following conversation segment:\n")
	} else {
		sb.WriteString("Summarize the following conversation:\n")
	}
	for _, message := range messages {
		fmt.Fprintf(&sb, "%s: %s\n", message.Role, message.Content)
	}
	return sb.String()
}

func compactionRenderedPayloadHash(payload *CompactionRenderedPayload) (string, error) {
	data, err := json.Marshal(struct {
		SystemPrompt  string                   `json:"system_prompt"`
		UserPrompt    string                   `json:"user_prompt"`
		CandidateRefs []contextfrag.ContextRef `json:"candidate_refs"`
	}{
		SystemPrompt:  payload.SystemPrompt,
		UserPrompt:    payload.UserPrompt,
		CandidateRefs: payload.CandidateRefs,
	})
	if err != nil {
		return "", fmt.Errorf("marshal compaction rendered payload: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
