package application

import (
	"testing"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/turn"
)

func TestHistorySourceMessageIDsFollowRetainedDatabaseMessages(t *testing.T) {
	row := func(id, role, text string) historyfrag.HistoryRecord {
		return historyfrag.HistoryRecord{
			DBMessageID: id,
			ModelMessage: ModelMessage{
				Role:          role,
				Content:       newTextContent(text),
				ContextSource: &turn.ContextMessageSource{Kind: "history", ID: id},
			},
		}
	}
	records := []historyfrag.HistoryRecord{
		row("00000000-0000-0000-0000-000000000001", "user", "first"),
		row("00000000-0000-0000-0000-000000000002", "assistant", "second"),
	}
	// Rendered from the first row, but not as stored: a fork must copy its
	// bytes rather than reference the row.
	rewritten := records[0].ModelMessage
	rewritten.Content = newTextContent("first, rewritten")
	messages := []ModelMessage{
		{Role: "system", Content: newTextContent("runtime workspace context")},
		rewritten,
		records[1].ModelMessage,
		{Role: "user", Content: newTextContent("current query")},
	}

	got := historySourceMessageIDsForMessages(messages, records)
	want := []string{"", "", records[1].DBMessageID, ""}
	if len(got) != len(want) {
		t.Fatalf("source IDs = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("source ID %d = %q, want %q", i, got[i], want[i])
		}
	}
}
