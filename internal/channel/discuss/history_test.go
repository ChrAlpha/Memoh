package discuss

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type lightweightHistoryStub struct {
	messages  []messagepkg.Message
	truncated bool
	sessionID string
	maxBytes  int64
	after     []messagepkg.HistoryPosition
}

func (s *lightweightHistoryStub) ListTurnResponseSourcesWithinBytes(_ context.Context, sessionID string, after messagepkg.HistoryPosition, maxBytes int64) (messagepkg.HistoryLoad, error) {
	s.sessionID = sessionID
	s.after = append(s.after, after)
	s.maxBytes = maxBytes
	return messagepkg.HistoryLoad{Messages: s.messages, Truncated: s.truncated}, nil
}

// A session's turn responses reach Discuss from after its history checkpoint
// on; a session without one is read from the start.
func TestLightweightHistoryLoadsAfterTheCheckpoint(t *testing.T) {
	for name, after := range map[string]messagepkg.HistoryPosition{
		"checkpoint":    {Turn: 12, Seq: 3},
		"no checkpoint": {},
	} {
		t.Run(name, func(t *testing.T) {
			source := &lightweightHistoryStub{}
			reader := discussHistoryReader{messages: source, maxBytes: 4000}
			if _, _, err := reader.Load(context.Background(), "session", after); err != nil {
				t.Fatal(err)
			}
			if len(source.after) != 1 || source.after[0] != after {
				t.Fatalf("history read after %v; want %+v", source.after, after)
			}
		})
	}
}

// The window reports what it admitted against the budget and whether the
// budget ended it, so a trigger logs a truncated load without measuring the
// history the budget left out.
func TestLightweightHistoryReportsItsWindow(t *testing.T) {
	answer := messagepkg.Message{
		ID: "answer", Role: "assistant", CreatedAt: time.Unix(1, 0),
		Content: []byte(`{"role":"assistant","content":"answer"}`),
	}
	for _, truncated := range []bool{false, true} {
		source := &lightweightHistoryStub{messages: []messagepkg.Message{answer, answer}, truncated: truncated}
		reader := discussHistoryReader{messages: source, maxBytes: 4000}
		_, window, err := reader.Load(context.Background(), "session", messagepkg.HistoryPosition{})
		if err != nil {
			t.Fatal(err)
		}
		want := discussHistoryWindow{Loaded: 2, Bytes: int64(2 * len(answer.Content)), MaxBytes: 4000, Truncated: truncated}
		if window != want {
			t.Fatalf("window = %+v, want %+v", window, want)
		}
	}
}

func TestDiscussLogsOnlyATruncatedHistoryLoad(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		var logs bytes.Buffer
		driver := NewDiscussDriver(DiscussDriverDeps{
			MessageService: &lightweightHistoryStub{truncated: truncated},
			Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
		})
		if _, err := driver.loadHistory(t.Context(), DiscussSessionConfig{ThreadID: "session"}, nil, driver.logger); err != nil {
			t.Fatal(err)
		}
		if logged := strings.Contains(logs.String(), "path=discuss_history_load"); logged != truncated {
			t.Fatalf("truncated=%t: history load logged=%t: %s", truncated, logged, logs.String())
		}
	}
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
			entries, window, err := reader.Load(context.Background(), "session", messagepkg.HistoryPosition{})
			if err != nil {
				t.Fatal(err)
			}
			if source.sessionID != "session" || source.maxBytes != 4000000 {
				t.Fatal("history request lost session/budget")
			}
			if window.Loaded != len(source.messages) {
				t.Fatalf("window loaded %d messages, want %d", window.Loaded, len(source.messages))
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
