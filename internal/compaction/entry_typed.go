package compaction

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/memohai/memoh/internal/historyfrag"
)

const entryMetadataMaxBytes = 256

var entryMetadataValueEscaper = strings.NewReplacer("[", "(", "]", ")")

type recordEntryPart struct {
	Type     string          `json:"type"`
	ToolName string          `json:"toolName"`
	Output   json.RawMessage `json:"output"`
	Result   json.RawMessage `json:"result"`
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
