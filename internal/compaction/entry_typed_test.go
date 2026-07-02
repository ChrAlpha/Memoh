package compaction

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
)

func renderSingleCandidatePrompt(t *testing.T, record historyfrag.HistoryRecord) string {
	t.Helper()
	payload, err := contextViewCompactionPrompt([]RecordCompactionCandidate{{Record: record}}, nil)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}
	return payload.UserPrompt
}

func TestRenderRecordEntryIncludesDirectedSignalHeader(t *testing.T) {
	t.Parallel()

	record := testRecord("row-1", "user", "please handle this", 0)
	record.Scope.CurrentMessageID = "tg-42"
	record.Scope.ReplyToMessageID = "tg-41"
	record.Scope.DisplayName = "Alice"
	record.Scope.Platform = "telegram"
	record.Scope.ConversationType = "group"
	record.Scope.ConversationName = "Ops Room"
	record.Scope.ReplyTarget = "thread-9"

	got := renderSingleCandidatePrompt(t, record)
	for _, want := range []string{
		"[message_id: tg-42]",
		"[reply_to: tg-41]",
		"[sender: Alice]",
		"[platform: telegram]",
		"[conversation_type: group]",
		"[conversation_name: Ops Room]",
		"[reply_target: thread-9]",
		"please handle this",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("entry missing %q:\n%s", want, got)
		}
	}
}

func TestRenderRecordEntryScrubsMediaAndBoundsToolOutput(t *testing.T) {
	t.Parallel()

	blob := strings.Repeat("QUJD", 100)
	record := testRecord("tool-row", "tool", "", 0)
	record.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolName":   "screenshot",
		"toolCallId": "call-1",
		"output": map[string]any{
			"content": []map[string]any{{"type": "image", "data": blob}},
		},
	}})

	got := renderSingleCandidatePrompt(t, record)
	if strings.Contains(got, blob) || strings.Contains(got, "QUJDQUJDQUJD") {
		t.Fatalf("base64 media leaked into summarizer input: %q", got[:80])
	}
	if !strings.Contains(got, "[media]") {
		t.Fatalf("expected media marker: %q", got)
	}
}

func TestRenderRecordEntryPreservesStructuredToolOutcome(t *testing.T) {
	t.Parallel()

	record := historyfrag.HistoryRecord{
		ModelMessage: conversation.ModelMessage{
			Role: "tool",
			Content: mustCompactionJSON([]map[string]any{{
				"type":       "tool-result",
				"toolName":   "apply_patch",
				"toolCallId": "call-1",
				"output":     map[string]any{"exit_code": 0, "files_changed": 3},
			}}),
		},
	}

	got := renderSingleCandidatePrompt(t, record)
	if !strings.Contains(got, "files_changed") {
		t.Fatalf("structured outcome lost: %q", got)
	}
}
