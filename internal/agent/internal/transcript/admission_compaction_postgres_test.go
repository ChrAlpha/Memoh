package transcript

import (
	"slices"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// refusedClaims counts the refused rows of the session that a compaction has
// claimed, which no refused row should ever be.
func (s *stage4Session) refusedClaims() int {
	s.T.Helper()
	var claimed int
	if err := s.Pool.QueryRow(s.Ctx, `
		SELECT count(*) FROM bot_history_messages
		WHERE session_id = $1 AND admission <> 'admitted' AND compact_id IS NOT NULL`,
		s.SessionID).Scan(&claimed); err != nil {
		s.T.Fatal(err)
	}
	return claimed
}

func withoutRefused(labels []string) []string {
	return slices.DeleteFunc(slices.Clone(labels), func(label string) bool { return strings.HasPrefix(label, "refused") })
}

// Rows refused at admission are not summary input. A refused row between two
// rows the summary covers is simply not claimed: the summary covers the
// admitted rows on both sides, and nothing refused reaches the summarizer or
// the context that follows.
func TestPostgresCompactionLeavesRefusedRowsOutOfTheSummary(t *testing.T) {
	s := newStage4Session(t)
	s.Persist("user", "c00")
	s.Persist("assistant", "c01")
	s.persistRefused(messagepkg.AdmissionRejectedACL, "refused-acl")
	s.Persist("user", "c02")
	s.persistRefused(messagepkg.AdmissionRejectedRuntimeDriver, "refused-driver")
	s.Persist("assistant", "c03")
	s.PersistConversation("d", 8)
	s.persistRefused(messagepkg.AdmissionRejectedACL, "refused-late")
	s.Persist("user", "d-last")

	s.Compact(stage4KeepTokens)

	sent := s.Summarizer.RequestText()
	if !strings.Contains(sent, "c00") {
		t.Fatalf("the summarizer was not sent the admitted rows: %.200s", sent)
	}
	for _, label := range []string{"refused-acl", "refused-driver", "refused-late"} {
		if strings.Contains(sent, label) {
			t.Fatalf("the summarizer was sent the text of %q", label)
		}
	}
	if claimed := s.refusedClaims(); claimed != 0 {
		t.Fatalf("%d refused rows were claimed by the compaction", claimed)
	}
	covered, raw := s.CompactedLabels()
	if !slices.Contains(covered, "c03") || slices.ContainsFunc(covered, func(l string) bool { return strings.HasPrefix(l, "refused") }) {
		t.Fatalf("covered rows = %v, want the admitted rows on both sides of a refused row and no refused row", covered)
	}

	want := append([]string{"SUMMARY-1"}, withoutRefused(raw)...)
	if got := agenttest.CompactionLabels(s.Assemble(runreq.ChatRequest{}).Messages); !slices.Equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}

// Refused rows are never claimed, so they stay uncompacted for good and pile
// up at the oldest end of the session. Their bytes must not take the
// compaction read budget with them, or a session that has collected enough of
// them could never be compacted again.
func TestPostgresCompactionIsNotStarvedByRefusedRows(t *testing.T) {
	s := newStage4Session(t)
	// Well over the smallest compaction read budget, all older than the
	// conversation.
	for i := range 8 {
		s.persistRefusedPadded(messagepkg.AdmissionRejectedACL, "refused-"+string(rune('a'+i)), 15000)
	}
	s.PersistConversation("a", 10)

	s.Compact(stage4KeepTokens)

	if sent := s.Summarizer.RequestText(); strings.Contains(sent, "refused-") {
		t.Fatalf("the summarizer was sent refused text: %.200s", sent)
	}
	if claimed := s.refusedClaims(); claimed != 0 {
		t.Fatalf("%d refused rows were claimed by the compaction", claimed)
	}
	covered, raw := agenttest.RequireCompactionSplit(t, s.CompactionSession)
	if !slices.Contains(covered, "a-00") {
		t.Fatalf("covered rows = %v, want the conversation compacted", covered)
	}
	want := append([]string{"SUMMARY-1"}, withoutRefused(raw)...)
	if got := agenttest.CompactionLabels(s.Assemble(runreq.ChatRequest{}).Messages); !slices.Equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}
