package tools

import (
	"encoding/json"
	"testing"

	"github.com/memohai/memoh/internal/conversation"
)

func TestExtractTextContentSummarizesAssistantToolCalls(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal([]map[string]any{
		{"type": "reasoning", "text": "thinking"},
		{"type": "tool-call", "toolName": "read", "toolCallId": "call-1", "input": map[string]any{"path": "/tmp/a.txt"}},
		{"type": "tool-call", "toolName": "edit", "toolCallId": "call-2", "input": map[string]any{"path": "/tmp/a.txt"}},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	raw, err := json.Marshal(conversation.ModelMessage{
		Role:    "assistant",
		Content: content,
	})
	if err != nil {
		t.Fatalf("marshal model message: %v", err)
	}

	got := extractTextContent(raw)
	want := "[tool_call: read, edit]"
	if got != want {
		t.Fatalf("extractTextContent() = %q, want %q", got, want)
	}
}

func TestExtractTextContentSummarizesToolResults(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal([]map[string]any{
		{"type": "tool-result", "toolName": "search_messages", "toolCallId": "call-1", "result": map[string]any{"count": 3}},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	raw, err := json.Marshal(conversation.ModelMessage{
		Role:    "tool",
		Content: content,
	})
	if err != nil {
		t.Fatalf("marshal model message: %v", err)
	}

	got := extractTextContent(raw)
	want := "[tool_result: search_messages]"
	if got != want {
		t.Fatalf("extractTextContent() = %q, want %q", got, want)
	}
}
