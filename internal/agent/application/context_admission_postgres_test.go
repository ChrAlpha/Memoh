package application

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// persistRefused writes a user row the way channel inbound writes one refused
// at admission: an ordinary passive row carrying the admission metadata.
func (s *stage4Session) persistRefused(admission messagepkg.Admission, label string) string {
	s.t.Helper()
	return s.persistRefusedPadded(admission, label, 0)
}

// persistRefusedPadded is persistRefused with words filler words after the
// label, for a refused row that takes a noticeable share of a read budget.
func (s *stage4Session) persistRefusedPadded(admission messagepkg.Admission, label string, words int) string {
	s.t.Helper()
	metadata := map[string]any{}
	messagepkg.SetAdmission(metadata, admission)
	text := label + " " + strings.Repeat("padding ", words)
	content, err := json.Marshal(sdkMessagesToModelMessages([]sdk.Message{sdk.UserMessage(text)})[0])
	if err != nil {
		s.t.Fatal(err)
	}
	row, err := s.messages.Persist(s.ctx, messagepkg.PersistInput{
		BotID: s.botID, SessionID: s.sessionID, Role: "user", Content: content, DisplayText: label, Metadata: metadata,
	})
	if err != nil {
		s.t.Fatalf("persist %s: %v", label, err)
	}
	s.clock = s.clock.Add(time.Minute)
	if _, err := s.pool.Exec(s.ctx, `UPDATE bot_history_messages SET created_at = $2 WHERE id = $1`, row.ID, s.clock); err != nil {
		s.t.Fatal(err)
	}
	return row.ID
}

func (s *stage4Session) assembleFor(purpose contextPurpose, chat ChatRequest, budgetTokens int) (assembledContext, error) {
	s.t.Helper()
	chat.BotID, chat.ChatID, chat.ThreadID = s.botID, s.botID, s.sessionID
	return s.service.assembleContext(s.ctx, chatContextRequest(purpose, chat, budgetTokens))
}

func recordLabels(records []historyfrag.HistoryRecord) []string {
	labels := make([]string, 0, len(records))
	for _, record := range records {
		labels = append(labels, strings.Fields(strings.TrimSpace(record.ModelMessage.TextContent()) + " ")[0])
	}
	return labels
}

// Rows refused at admission stay out of the model context on every path that
// reads history: a new run, a continuation, the memory query, and the retry
// whose request row is re-read through the window because the load left it
// out. Real PostgreSQL rows.
func TestPostgresContextOmitsRefusedRowsOnEveryReadPath(t *testing.T) {
	s := newStage4Session(t)
	s.persist("user", "q0")
	s.persist("assistant", "a0")
	ask := s.persist("user", "ask")
	s.persistRefused(messagepkg.AdmissionRejectedACL, "refused-acl")
	s.persistRefused(messagepkg.AdmissionRejectedRuntimeDriver, "refused-driver")
	answer := s.persist("assistant", "answer")
	s.persist("user", "last")

	t.Run("run", func(t *testing.T) {
		got := stage4Labels(s.assemble(ChatRequest{}).Messages)
		if want := []string{"q0", "a0", "ask", "answer", "last"}; !slices.Equal(got, want) {
			t.Fatalf("run context = %v, want %v", got, want)
		}
	})
	t.Run("continuation", func(t *testing.T) {
		assembled, err := s.assembleFor(contextPurposeContinuation, ChatRequest{}, 100000)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := stage4Labels(assembled.Messages), []string{"q0", "a0", "ask", "answer", "last"}; !slices.Equal(got, want) {
			t.Fatalf("continuation context = %v, want %v", got, want)
		}
	})
	t.Run("memory query", func(t *testing.T) {
		assembled, err := s.assembleFor(contextPurposeMemoryQuery, ChatRequest{}, 100000)
		if err != nil {
			t.Fatal(err)
		}
		// Refused rows are left out before the read's row limit, so the two
		// refused rows between "ask" and "last" take none of its places.
		if got, want := recordLabels(assembled.Records), []string{"q0", "ask", "last"}; !slices.Equal(got, want) {
			t.Fatalf("memory query rows = %v, want %v", got, want)
		}
	})
	t.Run("retry window re-read", func(t *testing.T) {
		// One token of budget loads only the newest row, so the request row
		// is brought back by the window re-read, whose cutoff is the answer.
		assembled, err := s.assembleFor(contextPurposeRun, ChatRequest{
			RequiredHistoryMessageID:     ask,
			HistoryCutoffBeforeMessageID: answer,
		}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := stage4Labels(assembled.Messages), []string{"ask"}; !slices.Equal(got, want) {
			t.Fatalf("retry context = %v, want %v", got, want)
		}
	})
}

// A retry whose request row is one refused at admission has no row to keep:
// the run fails as it does for a row that is not visible, before any model call.
func TestPostgresContextFailsWhenRequiredRowWasRefused(t *testing.T) {
	s := newStage4Session(t)
	s.persist("user", "q0")
	refused := s.persistRefused(messagepkg.AdmissionRejectedACL, "refused-acl")
	s.persist("assistant", "answer")

	_, err := s.assembleFor(contextPurposeRun, ChatRequest{RequiredHistoryMessageID: refused}, 1)
	if err == nil || !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("assembleContext error = %v, want the refused request row reported as not visible", err)
	}
}

// A refused row larger than the whole history budget, the newest in the
// session, does not use the budget up: the readable rows behind it still load.
func TestPostgresContextBudgetIsNotSpentOnRefusedRows(t *testing.T) {
	s := newStage4Session(t)
	s.persist("user", "q0")
	s.persist("assistant", "a0")
	s.persist("user", "q1")
	s.persistRefusedPadded(messagepkg.AdmissionRejectedACL, "refused-large", 4000)

	assembled, err := s.assembleFor(contextPurposeRun, ChatRequest{}, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stage4Labels(assembled.Messages), []string{"q0", "a0", "q1"}; !slices.Equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
	if assembled.Truncated {
		t.Fatal("context reported truncated: the refused row took budget from the readable history")
	}
}
