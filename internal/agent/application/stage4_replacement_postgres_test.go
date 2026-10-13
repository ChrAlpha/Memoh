package application

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/context/compaction"
)

// promptRecorder keeps the body of every summarizer request, so a test can
// read what the summarizer was told.
type promptRecorder struct {
	inner  *stage4Summarizer
	bodies []string
}

func (p *promptRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		p.bodies = append(p.bodies, string(body))
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	return p.inner.RoundTrip(req)
}

// A replacement's compaction summarizes only the history before the replaced
// turn. The first summary covers rows from the replaced turn on, so it is not
// carried into the replacement's prompt, and the rows after the cutoff stay
// raw. The control run without the cutoff shows the same session would
// otherwise summarize those rows with that summary as prior context (O8,
// real PostgreSQL artifacts).
func TestStage4ReplacementCompactionIgnoresSummariesOfTheReplacedTurn(t *testing.T) {
	s := newStage4Session(t)
	ids := s.persistConversation("a", 8)

	if res := s.compact(1); res.Status != "ok" {
		t.Fatalf("first compaction status = %q, want ok", res.Status)
	}
	covered, _ := s.compactedLabels()
	if len(covered) == 0 {
		t.Fatal("first compaction covered no rows")
	}
	s.persistConversation("b", 4)

	// The retry replaces the turn that starts at a-05; rows from a-05 on are
	// out of the replacement's scope.
	var cutoffMs int64
	if err := s.pool.QueryRow(s.ctx,
		`SELECT floor(extract(epoch from created_at) * 1000)::bigint FROM bot_history_messages WHERE id = $1`,
		ids[5]).Scan(&cutoffMs); err != nil {
		t.Fatalf("read cutoff row: %v", err)
	}

	calls := s.summarizer.callCount()
	cutoffCfg := s.triggerConfig(1)
	cutoffCfg.HistoryCutoff = &compaction.HistoryCutoff{BeforeMs: cutoffMs}
	res, err := s.compaction.RunCompactionSync(s.ctx, cutoffCfg)
	if err != nil {
		t.Fatalf("replacement compaction: %v", err)
	}
	if res.Status != compaction.StatusNoop || res.Reason != compaction.ReasonNoCandidates {
		t.Fatalf("replacement compaction = %+v, want noop/%s: the only raw rows before the cutoff are already covered", res, compaction.ReasonNoCandidates)
	}
	if got := s.summarizer.callCount(); got != calls {
		t.Fatalf("replacement compaction called the summarizer %d times, want none", got-calls)
	}

	// Control: the same session without the cutoff summarizes the raw rows
	// after the first summary, and that summary is its prior context.
	recorder := &promptRecorder{inner: s.summarizer}
	controlCfg := s.triggerConfig(1)
	controlCfg.HTTPClient = &http.Client{Transport: recorder}
	if _, err := s.compaction.RunCompactionSync(s.ctx, controlCfg); err != nil {
		t.Fatalf("control compaction: %v", err)
	}
	if len(recorder.bodies) == 0 {
		t.Fatal("control compaction never reached the summarizer; the test would pass without the cutoff being exercised")
	}
	if !strings.Contains(recorder.bodies[len(recorder.bodies)-1], "SUMMARY-1") {
		t.Fatal("control compaction did not carry the first summary as prior context; the stale-prior case is not reproduced")
	}
}
