package compaction

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
)

const (
	toolOutputMaxBytes    = 2048
	entryMetadataMaxBytes = 256
)

var (
	dataURIRe                 = regexp.MustCompile(`data:[a-zA-Z0-9.+/-]+;base64,[A-Za-z0-9+/=]+`)
	base64BlobRe              = regexp.MustCompile(`[A-Za-z0-9+/_-]{256,}={0,2}`)
	entryMetadataValueEscaper = strings.NewReplacer("[", "(", "]", ")")
)

type recordEntryPart struct {
	Type     string          `json:"type"`
	ToolName string          `json:"toolName"`
	Output   json.RawMessage `json:"output"`
	Result   json.RawMessage `json:"result"`
}

func renderRecordCandidateEntry(record historyfrag.HistoryRecord) string {
	content := strings.TrimSpace(renderRecordEntryContent(record.ModelMessage))
	if content == "" {
		return ""
	}
	if header := renderRecordEntryHeader(record); header != "" {
		return header + "\n" + content
	}
	return content
}

func renderRecordEntryHeader(record historyfrag.HistoryRecord) string {
	var lines []string
	add := func(label, value string) {
		value = cleanEntryMetadataValue(value)
		if value == "" {
			return
		}
		lines = append(lines, "["+label+": "+value+"]")
	}
	add("message_id", record.ExternalMessageID)
	add("reply_to", record.SourceReplyToMessageID)
	add("sender", record.SenderDisplayName)
	add("platform", record.Platform)
	add("conversation_type", record.Scope.ConversationType)
	add("conversation_name", record.Scope.ConversationName)
	add("reply_target", record.Scope.ReplyTarget)
	return strings.Join(lines, "\n")
}

func cleanEntryMetadataValue(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	value = entryMetadataValueEscaper.Replace(value)
	return truncateBytes(value, entryMetadataMaxBytes)
}

func renderRecordEntryContent(mm conversation.ModelMessage) string {
	var segments []string
	if text := strings.TrimSpace(mm.TextContent()); text != "" {
		segments = append(segments, text)
	}

	sawToolCallPart := false
	for _, part := range parseRecordEntryParts(mm.Content) {
		switch {
		case part.Type == "image":
			segments = append(segments, "[image]")
		case part.Type == "file":
			segments = append(segments, "[file]")
		case strings.Contains(part.Type, "tool-call"), strings.Contains(part.Type, "tool_call"):
			sawToolCallPart = true
			segments = append(segments, toolCallMarker(part.ToolName))
		case strings.Contains(part.Type, "tool-result"), strings.Contains(part.Type, "tool_result"):
			segments = append(segments, renderToolResult(part.Output, part.Result))
		}
	}

	if !sawToolCallPart {
		for _, call := range mm.ToolCalls {
			segments = append(segments, toolCallMarker(call.Function.Name))
		}
	}

	return strings.Join(segments, "\n")
}

func parseRecordEntryParts(content json.RawMessage) []recordEntryPart {
	if len(content) == 0 {
		return nil
	}
	var parts []recordEntryPart
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	return parts
}

func toolCallMarker(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "[tool_call]"
	}
	return "[tool_call: " + name + "]"
}

func renderToolResult(candidates ...json.RawMessage) string {
	if s := firstOutputText(candidates...); s != "" {
		return sanitizeToolText(s)
	}
	if s := rawToolOutput(candidates...); s != "" {
		return sanitizeToolText(s)
	}
	return "[tool result]"
}

func sanitizeToolText(s string) string {
	s = dataURIRe.ReplaceAllString(s, "[media]")
	s = base64BlobRe.ReplaceAllString(s, "[media]")
	return truncateBytes(s, toolOutputMaxBytes)
}

func firstOutputText(candidates ...json.RawMessage) string {
	for _, raw := range candidates {
		if s := outputText(raw); s != "" {
			return s
		}
	}
	return ""
}

func outputText(raw json.RawMessage) string {
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

func rawToolOutput(candidates ...json.RawMessage) string {
	for _, raw := range candidates {
		s := strings.TrimSpace(string(raw))
		if s == "" || s == "null" {
			continue
		}
		return s
	}
	return ""
}

func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " …[truncated]"
}
