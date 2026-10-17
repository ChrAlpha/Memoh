package application

import (
	"strings"
	"testing"
)

// A long session's later round reads what the earlier one compacted, in order:
// the summary stands where the rows it covers were, the rows after it keep
// their sequence, and the round's own message comes last. Compaction happens
// before the run, so the round being measured reads a boundary it did not
// create.
func TestR4bLaterRoundReadsCompactedHistoryInOrder(t *testing.T) {
	s, fixture, recorder := stage4DriveSession(t)
	s.Padding = 3000
	s.PersistConversation("a", 8)

	startStage4Turn(t, fixture, "first question")
	if got := s.Summarizer.CallCount(); got == 0 {
		t.Fatal("the first round did not compact; the test would assert on a boundary it never built")
	}
	afterFirst := len(recorder.recorded())

	startStage4Turn(t, fixture, "second question")
	requests := recorder.recorded()
	if len(requests) <= afterFirst {
		t.Fatalf("the second round reached the provider %d times, want more than the first round's %d", len(requests), afterFirst)
	}

	body := string(marshalDriveRequests(t, requests[afterFirst:afterFirst+1]))
	covered, raw := s.CompactedLabels()
	if len(covered) == 0 {
		t.Fatalf("the session has nothing compacted to read: covered=%v raw=%v", covered, raw)
	}

	// Nothing the summary stands for is sent again.
	for _, label := range covered {
		if strings.Contains(body, label+" padding") {
			t.Fatalf("the second round still carries covered row %s:\n%s", label, body)
		}
	}
	// The summary stands where they were, and the rows after it follow in
	// order.
	at := strings.Index(body, "SUMMARY-")
	if at < 0 {
		t.Fatalf("the second round carries no summary:\n%s", body)
	}
	last := at
	for _, label := range raw {
		found := strings.Index(body, label+" padding")
		if found < 0 {
			t.Fatalf("the second round lost row %s:\n%s", label, body)
		}
		if found < last {
			t.Fatalf("row %s comes before the position it should follow:\n%s", label, body)
		}
		last = found
	}
	// The round's own message is the last thing in the history it reads.
	if question := strings.Index(body, "second question"); question < last {
		t.Fatalf("the second round's own message does not follow the history:\n%s", body)
	}
}
