package application

import (
	"slices"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// refusedClaims counts the refused rows of the session that a compaction has
// claimed, which no refused row should ever be.
func (s *stage4Session) refusedClaims() int {
	s.t.Helper()
	var claimed int
	if err := s.pool.QueryRow(s.ctx, `
		SELECT count(*) FROM bot_history_messages
		WHERE session_id = $1 AND metadata->>'admission' IS NOT NULL AND compact_id IS NOT NULL`,
		s.sessionID).Scan(&claimed); err != nil {
		s.t.Fatal(err)
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
	s.persist("user", "c00")
	s.persist("assistant", "c01")
	s.persistRefused(messagepkg.AdmissionRejectedACL, "refused-acl")
	s.persist("user", "c02")
	s.persistRefused(messagepkg.AdmissionRejectedRuntimeDriver, "refused-driver")
	s.persist("assistant", "c03")
	s.persistConversation("d", 8)
	s.persistRefused(messagepkg.AdmissionRejectedACL, "refused-late")
	s.persist("user", "d-last")

	s.compact(stage4KeepTokens)

	sent := s.summarizer.requestText()
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
	covered, raw := s.compactedLabels()
	if !slices.Contains(covered, "c03") || slices.ContainsFunc(covered, func(l string) bool { return strings.HasPrefix(l, "refused") }) {
		t.Fatalf("covered rows = %v, want the admitted rows on both sides of a refused row and no refused row", covered)
	}

	want := append([]string{"SUMMARY-1"}, withoutRefused(raw)...)
	if got := stage4Labels(s.assemble(runreq.ChatRequest{}).Messages); !slices.Equal(got, want) {
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
	s.persistConversation("a", 10)

	s.compact(stage4KeepTokens)

	if sent := s.summarizer.requestText(); strings.Contains(sent, "refused-") {
		t.Fatalf("the summarizer was sent refused text: %.200s", sent)
	}
	if claimed := s.refusedClaims(); claimed != 0 {
		t.Fatalf("%d refused rows were claimed by the compaction", claimed)
	}
	covered, raw := requireStage4Split(t, s)
	if !slices.Contains(covered, "a-00") {
		t.Fatalf("covered rows = %v, want the conversation compacted", covered)
	}
	want := append([]string{"SUMMARY-1"}, withoutRefused(raw)...)
	if got := stage4Labels(s.assemble(runreq.ChatRequest{}).Messages); !slices.Equal(got, want) {
		t.Fatalf("context = %v, want %v", got, want)
	}
}
