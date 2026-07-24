package contextview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
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
	EntryCount    int
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
		if header := renderCompactionFragHeader(frag); header != "" {
			content = header + "\n" + content
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
		EntryCount:    len(entries),
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
	texts := make([]string, 0, len(frag.Parts))
	markers := make([]string, 0, len(frag.Parts))
	for _, part := range frag.Parts {
		switch part.Type {
		case contextfrag.PartText:
			if strings.TrimSpace(part.Text) != "" {
				texts = append(texts, part.Text)
			}
		case contextfrag.PartImage:
			markers = append(markers, "[image]")
		case contextfrag.PartSDKMessage:
			if msg := sdkMessagePart(part); msg != nil {
				msgTexts, msgMarkers := splitCompactionMessageContent(*msg)
				texts = append(texts, msgTexts...)
				markers = append(markers, msgMarkers...)
			}
		}
	}
	segments := make([]string, 0, 1+len(markers))
	if block := strings.TrimSpace(strings.Join(texts, "\n")); block != "" {
		segments = append(segments, block)
	}
	segments = append(segments, markers...)
	return strings.Join(segments, "\n")
}

func splitCompactionMessageContent(msg sdk.Message) ([]string, []string) {
	texts := make([]string, 0, len(msg.Content))
	markers := make([]string, 0, len(msg.Content))
	for _, part := range msg.Content {
		switch p := part.(type) {
		case sdk.TextPart:
			if strings.TrimSpace(p.Text) != "" {
				texts = append(texts, p.Text)
			}
		case sdk.ImagePart:
			markers = append(markers, "[image]")
		case sdk.FilePart:
			markers = append(markers, "[file]")
		case sdk.ToolCallPart:
			markers = append(markers, compactionToolCallMarker(p.ToolName))
		case sdk.ToolResultPart:
			markers = append(markers, compactionToolResultText(p.Result))
		}
	}
	return texts, markers
}

var (
	compactionDataURIRe    = regexp.MustCompile(`data:[a-zA-Z0-9.+/-]+;base64,[A-Za-z0-9+/=]+`)
	compactionBase64BlobRe = regexp.MustCompile(`[A-Za-z0-9+/_-]{256,}={0,2}`)

	compactionMetadataValueEscaper = strings.NewReplacer("[", "(", "]", ")")
)

const (
	compactionToolOutputMaxBytes = 2048
	compactionMetadataMaxBytes   = 256
)

func renderCompactionFragHeader(frag contextfrag.ContextFrag) string {
	scope := frag.Scope
	var lines []string
	add := func(label, value string) {
		value = cleanCompactionMetadataValue(value)
		if value == "" {
			return
		}
		lines = append(lines, "["+label+": "+value+"]")
	}
	add("message_id", scope.CurrentMessageID)
	add("reply_to", scope.ReplyToMessageID)
	add("sender", scope.DisplayName)
	add("platform", scope.Platform)
	add("conversation_type", scope.ConversationType)
	add("conversation_name", scope.ConversationName)
	add("reply_target", scope.ReplyTarget)
	return strings.Join(lines, "\n")
}

func cleanCompactionMetadataValue(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	value = compactionMetadataValueEscaper.Replace(value)
	return truncateCompactionBytes(value, compactionMetadataMaxBytes)
}

func sanitizeCompactionToolText(s string) string {
	s = compactionDataURIRe.ReplaceAllString(s, "[media]")
	s = compactionBase64BlobRe.ReplaceAllString(s, "[media]")
	return truncateCompactionBytes(s, compactionToolOutputMaxBytes)
}

func truncateCompactionBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " \u2026[truncated]"
}

func compactionOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Value   string `json:"value"`
		Text    string `json:"text"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if value := strings.TrimSpace(obj.Value); value != "" {
			return value
		}
		if text := strings.TrimSpace(obj.Text); text != "" {
			return text
		}
		var texts []string
		for _, content := range obj.Content {
			if text := strings.TrimSpace(content.Text); text != "" {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return ""
}

func compactionToolCallMarker(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "[tool_call]"
	}
	return "[tool_call: " + name + "]"
}

func compactionToolResultText(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "[tool result]"
	}
	if s := compactionOutputText(raw); s != "" {
		return sanitizeCompactionToolText(s)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "[tool result]"
	}
	return sanitizeCompactionToolText(trimmed)
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
