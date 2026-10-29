package discuss

import (
	"context"
	"strings"
	"testing"
	"time"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type lightweightHistoryStub struct {
	messages  []messagepkg.Message
	sessionID string
	maxBytes  int64
}

func (s *lightweightHistoryStub) ListTurnResponseSourcesSinceBySessionWithinBytes(_ context.Context, sessionID string, _ time.Time, maxBytes int64) ([]messagepkg.Message, error) {
	s.sessionID = sessionID
	s.maxBytes = maxBytes
	return s.messages, nil
}

func TestLightweightHistoryPreservesInterruptedReasoning(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted", true: "completed"}[completed], func(t *testing.T) {
			source := &lightweightHistoryStub{messages: []messagepkg.Message{{
				ID: "checkpoint", Role: "assistant", CreatedAt: time.Unix(1, 0),
				Content:  []byte(`{"role":"assistant","content":[{"type":"reasoning","text":"unfinished thought"}]}`),
				Metadata: map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true},
			}}}
			if completed {
				source.messages = append(source.messages, messagepkg.Message{
					ID: "answer", Role: "assistant", CreatedAt: time.Unix(2, 0),
					Content: []byte(`{"role":"assistant","content":"completed answer"}`),
				})
			}
			reader := discussHistoryReader{messages: source, maxBytes: 4000000}
			entries, window, err := reader.Load(context.Background(), "session", nil)
			if err != nil {
				t.Fatal(err)
			}
			if source.sessionID != "session" || source.maxBytes != 4000000 {
				t.Fatal("history request lost session/budget")
			}
			if window.Loaded != len(source.messages) || window.BudgetReached() {
				t.Fatalf("incorrect history window: %+v", window)
			}
			var text strings.Builder
			for _, entry := range entries {
				text.Write(entry.RawContent)
			}
			if strings.Contains(text.String(), "unfinished thought") == completed {
				t.Fatalf("reasoning checkpoint projection incorrect: %s", text.String())
			}
			if completed && !strings.Contains(text.String(), "completed answer") {
				t.Fatal("completed answer missing")
			}
		})
	}
}

// The window reports reaching its budget from what it loaded, the same
// content bytes the database walk spends, so the admission log no longer
// needs a whole-session aggregate.
func TestLightweightHistoryWindowReportsBudget(t *testing.T) {
	content := []byte(`{"role":"assistant","content":"answer"}`)
	source := &lightweightHistoryStub{messages: []messagepkg.Message{
		{ID: "a", Role: "assistant", CreatedAt: time.Unix(1, 0), Content: content},
		{ID: "b", Role: "assistant", CreatedAt: time.Unix(2, 0), Content: content},
	}}
	for _, tc := range []struct {
		maxBytes int64
		reached  bool
	}{
		{maxBytes: int64(2 * len(content)), reached: true},
		{maxBytes: int64(2*len(content)) + 1, reached: false},
	} {
		_, window, err := discussHistoryReader{messages: source, maxBytes: tc.maxBytes}.Load(context.Background(), "session", nil)
		if err != nil {
			t.Fatal(err)
		}
		if window.Bytes != int64(2*len(content)) || window.BudgetReached() != tc.reached {
			t.Fatalf("maxBytes=%d window=%+v, want reached=%v", tc.maxBytes, window, tc.reached)
		}
	}
}

// A response a summary replaces is not decoded, yet still counts as the
// answer that closes an earlier interrupted checkpoint.
func TestLightweightHistorySkipsCoveredResponses(t *testing.T) {
	source := &lightweightHistoryStub{messages: []messagepkg.Message{
		{
			ID: "checkpoint", Role: "assistant", CreatedAt: time.Unix(1, 0),
			Content:  []byte(`{"role":"assistant","content":[{"type":"reasoning","text":"unfinished thought"},{"type":"text","text":"partial"}]}`),
			Metadata: map[string]any{messagepkg.AgentStepInterruptedMetadataKey: true},
		},
		{ID: "covered", Role: "assistant", CreatedAt: time.Unix(2, 0), Content: []byte(`{"role":"assistant","content":"covered answer"}`)},
	}}
	entries, window, err := discussHistoryReader{messages: source, maxBytes: 4000000}.Load(context.Background(), "session", []string{"covered"})
	if err != nil {
		t.Fatal(err)
	}
	if window.Loaded != 2 || len(entries) != 1 || entries[0].SourceMessageID != "checkpoint" {
		t.Fatalf("window=%+v entries=%+v, want only the uncovered response decoded", window, entries)
	}
	if strings.Contains(string(entries[0].RawContent), "unfinished thought") {
		t.Fatalf("the covered answer closed the checkpoint, its reasoning must stay hidden: %s", entries[0].RawContent)
	}
}
