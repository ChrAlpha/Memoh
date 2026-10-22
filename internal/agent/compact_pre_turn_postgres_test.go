package agent

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
)

// Context at the blocking share of the window is compacted once, before the
// provider request, and the request carries the summary in place of the rows
// it covers (ruling 1, the pre-turn synchronous compaction, K2). The history
// is real PostgreSQL rows, the compaction is the real service, and only the
// summarizer is a stand-in.
func TestPreTurnCompactionReplacesCoveredRowsInTheProviderRequest(t *testing.T) {
	s, fixture, recorder := compactionDriveSession(t)
	s.Padding = 3000
	s.PersistConversation("a", 8)

	startCompactionTurn(t, fixture, "what now?")

	if got := s.Summarizer.CallCount(); got != 1 {
		t.Fatalf("summarizer calls = %d, want exactly one synchronous compaction", got)
	}
	covered, raw := agenttest.RequireCompactionSplit(t, s)
	requests := requireDriveRequests(t, recorder)
	body := string(marshalDriveRequests(t, requests[:1]))
	for _, label := range covered {
		if strings.Contains(body, label+" padding") {
			t.Fatalf("the provider request still carries covered row %s", label)
		}
	}
	last := strings.Index(body, "SUMMARY-1")
	if last < 0 {
		t.Fatalf("the provider request has no summary:\n%s", body)
	}
	for _, label := range raw {
		at := strings.Index(body, label+" padding")
		if at < 0 {
			t.Fatalf("the provider request lost uncovered row %s", label)
		}
		if at < last {
			t.Fatalf("uncovered row %s comes before the summary", label)
		}
		last = at
	}
	if !strings.Contains(body, "what now?") {
		t.Fatal("the provider request lacks the current message")
	}
}

// A summarizer that fails does not stop the turn: the request still reaches
// the provider, no row is left claimed by a failed summary, and the context
// the turn goes on with is the history as it was (ruling 4, what does not fit
// goes to the provider as it is): every row reaches the request, in order.
func TestPreTurnCompactionFailureSendsTheContextAsIs(t *testing.T) {
	s, fixture, recorder := compactionDriveSession(t)
	s.Padding = 3000
	ids := s.PersistConversation("a", 8)
	s.Summarizer.RefuseWith(http.StatusBadRequest)

	startCompactionTurn(t, fixture, "what now?")

	if s.Summarizer.CallCount() == 0 {
		t.Fatal("the compaction never reached the summarizer; the test would pass without one")
	}
	body := string(marshalDriveRequests(t, requireDriveRequests(t, recorder)[:1]))
	if strings.Contains(body, "SUMMARY-") {
		t.Fatalf("the request carries a summary the summarizer never produced")
	}
	last := -1
	for i := range ids {
		label := fmt.Sprintf("a-%02d padding", i)
		at := strings.Index(body, label)
		if at < 0 {
			t.Fatalf("the request lost row %s:\n%s", label, body)
		}
		if at < last {
			t.Fatalf("row %s is out of order", label)
		}
		last = at
	}
	if at := strings.Index(body, "what now?"); at < last {
		t.Fatal("the request lacks the current message after the history")
	}
	if strings.Contains(body, "trimmed to fit") {
		t.Fatal("the request carries a trim notice")
	}
	if covered, raw := s.CompactedLabels(); len(covered) != 0 || len(raw) != len(ids) {
		t.Fatalf("a failed compaction left covered=%v raw=%v", covered, raw)
	}

	req := runreq.ChatRequest{BotID: s.BotID, ChatID: s.BotID, ThreadID: s.SessionID, RunID: "pre-turn-compaction-run"}
	assembled, err := fixture.service.transcript.Assemble(s.Ctx, transcript.ForChat(transcript.PurposeRun, runreq.ChatRequest{BotID: s.BotID, ChatID: s.BotID, ThreadID: s.SessionID}, 100000))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	before := agenttest.CompactionLabels(assembled.Messages)
	after, err := fixture.service.compact.BeforeTurn(s.Ctx, req, "", 60000, assembled, fixture.service.compact.NewEstimate(s.Ctx, req.ThreadID, nil))
	if err != nil {
		t.Fatalf("compactBeforeTurn: %v", err)
	}
	if got := agenttest.CompactionLabels(after.Messages); !slices.Equal(got, before) || len(before) != len(ids) {
		t.Fatalf("context after the failed backstop = %v, want the %d rows as they were (%v)", got, len(ids), before)
	}
}
