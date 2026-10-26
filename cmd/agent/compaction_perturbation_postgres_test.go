package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// An input larger than the window cannot be sent: its turn fails with an
// explicit code, and the turns after it answer. A Discuss session keeps the
// input it could not answer in its history; the next turn folds it, by its
// head and tail, into the summary it renders.
func TestPostgresAnOversizedInputFailsOnlyItsOwnTurn(t *testing.T) {
	for _, name := range []string{"web", "discuss"} {
		t.Run(name, func(t *testing.T) {
			entry := runEntryNamed(name)
			s := newEffectLoop(t, entry)
			if code, _ := s.run(entry, "EARLIER_TURN_KEEP", 0); code != "" {
				t.Fatalf("earlier turn failed with %s", code)
			}
			const payload = "HUGE_PAYLOAD "
			if code, _ := s.run(entry, payload+strings.Repeat("o", 40000), 0); code == "" {
				t.Fatal("the oversized turn answered, want it to fail with an explicit code")
			}
			for i := range 2 {
				code, requests := s.run(entry, fmt.Sprintf("NEXT_TURN_%d", i), 0)
				if code != "" || len(requests) != 1 || strings.Contains(requests[0], payload) {
					t.Fatalf("turn %d after the oversized input: code=%q provider_calls=%d, want one call without the payload", i, code, len(requests))
				}
				if name == "discuss" && len(summaryContents(t, requests[0])) != 1 {
					t.Fatalf("turn %d after the oversized input renders no summary, want the input folded", i)
				}
			}
		})
	}
}

// A model the session switches to with a smaller window renders the same
// history: the next turn folds what no longer fits before its first step and
// answers, and the turns after it replay that summary.
func TestPostgresASmallerWindowFoldsTheHistoryItNoLongerHolds(t *testing.T) {
	web := runEntryNamed("web")
	s := newEffectLoop(t, web)
	f := s.f
	s.window(16000)
	for i := range 8 {
		if code, _ := s.run(web, fmt.Sprintf("WIDE_TURN_%d ", i)+strings.Repeat("w", 2400), 0); code != "" {
			t.Fatalf("turn %d in the wide window failed with %s", i, code)
		}
	}
	if f.summaries.Load() != 0 {
		t.Fatalf("the wide window compacted %d times, want the history to fit it", f.summaries.Load())
	}
	s.window(8000)
	code, requests := s.run(web, "FIRST_NARROW_TURN", 0)
	if code != "" || len(requests) != 1 || f.summaries.Load() == 0 || len(summaryContents(t, requests[0])) != 1 || strings.Contains(requests[0], "WIDE_TURN_0 ") {
		t.Fatalf("first turn in the narrow window: code=%q provider_calls=%d summary_calls=%d, want the history folded before its first step", code, len(requests), f.summaries.Load())
	}
	summaries := f.summaries.Load()
	if code, requests := s.run(web, "SECOND_NARROW_TURN", 0); code != "" || len(requests) != 1 || f.summaries.Load() != summaries {
		t.Fatalf("second turn in the narrow window: code=%q summary_calls=%d, want the summary replayed", code, f.summaries.Load()-summaries)
	}
}

// Effects a step calls at once share its bound: four results that alone
// would each fill most of the room reach the next request cut to their
// shares, and the run goes on to fold and finish.
func TestPostgresAParallelStepSharesItsBound(t *testing.T) {
	web := runEntryNamed("web")
	s := newEffectLoop(t, web)
	s.parallel, s.resultBytes = 4, 6000
	code, requests := s.run(web, loopInput, 12)
	if code != "" || len(s.effects) != 12 || len(requests) == 0 {
		t.Fatalf("code=%q effects=%v, want the three parallel steps run and the turn answered", code, s.effects)
	}
	shared := 0
	for _, message := range requestMessages(t, requests[1]) {
		if strings.Contains(message, `"role":"tool"`) {
			if !strings.Contains(message, "[memoh pruned]") || len(message) > 4000 {
				t.Fatalf("a parallel result reached the next request at %d bytes, want it cut to its share", len(message))
			}
			shared++
		}
	}
	if shared != 4 {
		t.Fatalf("the request after the first step carries %d results, want 4", shared)
	}
}

// A turn that failed because its fold could not be summarized is retried
// once the summarizer is back: the retry folds and answers, and the session
// keeps one consistent checkpoint.
func TestPostgresARetryAfterAFailedFoldAnswers(t *testing.T) {
	web := runEntryNamed("web")
	s := newEffectLoop(t, web)
	f := s.f
	if code, _ := s.run(web, "EARLIER_TURN_KEEP", 0); code != "" {
		t.Fatalf("earlier turn failed with %s", code)
	}
	s.downAt = 1
	if code, _ := s.run(web, loopInput, 14); code != "context.budget_unsatisfied" {
		t.Fatalf("summarizer down: code=%q, want the fold to fail the turn", code)
	}
	s.downAt = 0
	f.failSummary.Store(false)
	s.mu.Lock()
	s.marker, s.steps, s.calls, s.effects, s.start = loopInput, 14, 0, nil, len(s.requests)
	s.mu.Unlock()
	if code := s.replaceLatest(s.replacement("")); code != "" || len(s.effects) != 14 {
		t.Fatalf("retry: code=%q effects=%v, want every effect run again and the turn answered", code, s.effects)
	}
	assertActiveCheckpointHolds(t, f.ctx, postgresstore.NewQueriesWithPool(f.pool, sqlc.New(f.pool)), f.sessionID)
}
