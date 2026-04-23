package pipeline

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/memohai/memoh/internal/conversation"
	messagepkg "github.com/memohai/memoh/internal/message"
)

func TestDecodeTurnResponseEntryUsesVisibleText(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal([]map[string]any{
		{"type": "reasoning", "text": "thinking"},
		{"type": "text", "text": "任务完成"},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	modelMessage, err := json.Marshal(conversation.ModelMessage{
		Role:    "assistant",
		Content: content,
	})
	if err != nil {
		t.Fatalf("marshal model message: %v", err)
	}

	entry, ok := DecodeTurnResponseEntry(messagepkg.Message{
		Role:      "assistant",
		Content:   modelMessage,
		CreatedAt: time.Unix(1710000000, 0).UTC(),
	})
	if !ok {
		t.Fatal("expected turn response entry")
	}
	if entry.Content != "任务完成" {
		t.Fatalf("content = %q, want %q", entry.Content, "任务完成")
	}
}

func TestDecodeTurnResponseEntrySkipsToolCallOnlyPayload(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal([]map[string]any{
		{"type": "reasoning", "text": "thinking"},
		{"type": "tool-call", "toolName": "read", "toolCallId": "call-1", "input": map[string]any{"path": "/tmp/a.txt"}},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	modelMessage, err := json.Marshal(conversation.ModelMessage{
		Role:    "assistant",
		Content: content,
	})
	if err != nil {
		t.Fatalf("marshal model message: %v", err)
	}

	if _, ok := DecodeTurnResponseEntry(messagepkg.Message{
		Role:      "assistant",
		Content:   modelMessage,
		CreatedAt: time.Unix(1710000000, 0).UTC(),
	}); ok {
		t.Fatal("expected tool-call-only payload to be skipped")
	}
}
