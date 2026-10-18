package compaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/hooks"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

func TestRunCompactionSyncReportsScopedNoop(t *testing.T) {
	// An already-compact/fresh session compacts nothing. The result must be a
	// scoped no-op, so a caller (the HTTP endpoint) never has to fall back to an
	// unscoped bot-wide log that could belong to another session.
	q := &fakeQueries{}
	stub := &stubModel{summary: "unused"}
	svc := newMachineryService(q)

	res, err := svc.RunCompactionSync(context.Background(), machineryConfig(stub, 100))
	if err != nil {
		t.Fatalf("no-op must not error: %v", err)
	}
	if res.Status != StatusNoop || res.MessageCount != 0 || res.Summary != "" {
		t.Fatalf("no-op result = %+v, want noop/0/empty", res)
	}
	if stub.calls != 0 || q.created {
		t.Fatalf("no-op must not call the model or create a log (calls=%d created=%v)", stub.calls, q.created)
	}
}

func TestRunCompactionSyncReportsScopedSummaryOnSuccess(t *testing.T) {
	q := &fakeQueries{uncompacted: machineryCorpus(t)}
	stub := &stubModel{summary: "SUMMARY-OK"}
	svc := newMachineryService(q)

	res, err := svc.RunCompactionSync(context.Background(), machineryConfig(stub, 450))
	if err != nil {
		t.Fatalf("RunCompactionSync: %v", err)
	}
	if res.Status != StatusOK || res.Summary != "SUMMARY-OK" || res.MessageCount != len(q.markedIDs) {
		t.Fatalf("success result = %+v, want ok/SUMMARY-OK/%d", res, len(q.markedIDs))
	}
}

func TestRunCompactionSyncFailureSurfacesError(t *testing.T) {
	q := &fakeQueries{uncompacted: machineryCorpus(t)}
	svc := newMachineryService(q)
	cfg := machineryConfig(&stubModel{}, 450)
	cfg.HTTPClient = &http.Client{Transport: &failingModel{}}

	res, err := svc.RunCompactionSync(context.Background(), cfg)
	if err == nil {
		t.Fatal("a summarizer failure must surface an error, not a stale result")
	}
	if res.Status == StatusOK {
		t.Fatalf("failed result must not report ok: %+v", res)
	}
}

func TestRunCompactionRetriesImmediatelyAfterFailure(t *testing.T) {
	q := &fakeQueries{uncompacted: machineryCorpus(t)}
	svc := newMachineryService(q)
	cfg := machineryConfig(&stubModel{}, 450)
	fail := &failingModel{}
	cfg.HTTPClient = &http.Client{Transport: fail}

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := svc.RunCompactionSync(context.Background(), cfg); err == nil {
			t.Fatalf("attempt %d must run and fail", attempt)
		}
		if fail.calls != attempt {
			t.Fatalf("attempt %d: calls = %d, a failure must not hold back the next attempt", attempt, fail.calls)
		}
	}

	stub := &stubModel{summary: "SUMMARY-OK"}
	cfg.HTTPClient = &http.Client{Transport: stub}
	if res, err := svc.RunCompactionSync(context.Background(), cfg); err != nil || res.Status != StatusOK {
		t.Fatalf("a fixed summarizer must succeed on the next attempt: res=%#v err=%v", res, err)
	}
}

func TestRunCompactionSyncSurfacesCompletionPersistenceFailure(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{uncompacted: machineryCorpus(t), completeErr: errors.New("db down")}
	svc := newMachineryService(q)
	stub := &stubModel{summary: "durable summary"}

	res, err := svc.RunCompactionSync(context.Background(), machineryConfig(stub, 200))
	if err == nil {
		t.Fatal("completion persistence failure must surface an error")
	}
	if res.Status == StatusOK {
		t.Fatal("result must not claim ok when the summary was never persisted")
	}
}

// summarizerWindowFor is a summarizer window whose input room is about
// inputTokens beside the default output allowance and the fixed prompt.
func summarizerWindowFor(inputTokens int) int {
	return (inputTokens + estimateBytesAsTokens(systemPrompt) + compactionPromptFramingTokens) * 4 / 3
}

func TestDoCompactionSharesTheSummarizerInputWithEarlierSummaries(t *testing.T) {
	t.Parallel()

	rows := make([]sqlc.ListUncompactedMessagesBySessionRow, 0, 13)
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		rows = append(rows, textRow(t, role, 100))
	}
	rows = append(rows, mkRow(t, "user", `"current"`, 40))

	run := func(t *testing.T, withPrior bool) int {
		t.Helper()
		stub := &stubModel{summary: "condensed"}
		cfg := machineryConfig(stub, 100000)
		cfg.SummaryWindowTokens = summarizerWindowFor(1000)
		cfg.ProtectedSources = historySources(rows[12])
		var priorLogs []sqlc.BotHistoryMessageCompact
		if withPrior {
			priorLogs = fusionParentLogs(t, cfg, strings.Repeat("p", 1200))
		}
		q := &fakeQueries{uncompacted: rows, priorLogs: priorLogs}
		res, err := newMachineryService(q).RunCompactionSync(context.Background(), cfg)
		if err != nil {
			t.Fatalf("RunCompactionSync: %v", err)
		}
		if res.Status != StatusProgress {
			t.Fatalf("status = %q, want %q: the rows past the summarizer input wait for the next pass", res.Status, StatusProgress)
		}
		if request := estimateBytesAsTokens(stub.prompt); request+stub.maxTokens > cfg.SummaryWindowTokens {
			t.Fatalf("request %d + max_tokens %d exceeds the %d window", request, stub.maxTokens, cfg.SummaryWindowTokens)
		}
		return len(q.markedIDs)
	}

	without := run(t, false)
	with := run(t, true)
	if with >= without {
		t.Fatalf("earlier summaries must share the summarizer input: marked %d with prior, %d without", with, without)
	}
}

func TestDoCompactionReadsAWholeEntryOnlyWhenTheRequestFitsTheWindow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		tokens  int
		earlier int
		fits    bool
	}{
		{name: "over half the window", tokens: 4000, fits: true},
		{name: "earlier summaries over half the window", tokens: 100, earlier: 4000, fits: true},
		{name: "past the window", tokens: 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := []sqlc.ListUncompactedMessagesBySessionRow{textRow(t, "user", tc.tokens), mkRow(t, "user", `"current"`, 40)}
			stub := &stubModel{summary: "condensed"}
			cfg := machineryConfig(stub, 100000)
			cfg.SummaryWindowTokens, cfg.MaxOutputTokens = 8000, 100000
			cfg.ProtectedSources = historySources(rows[1])
			q := &fakeQueries{uncompacted: rows}
			if tc.earlier > 0 {
				q.priorLogs = fusionParentLogs(t, cfg, strings.Repeat("p", tc.earlier*4))
			}
			_, err := newMachineryService(q).RunCompactionSync(context.Background(), cfg)
			if !tc.fits {
				if !errors.Is(err, errCompactionInputOverflow) || stub.calls != 0 || q.created || len(q.markedIDs) != 0 {
					t.Fatalf("err=%v calls=%d created=%t marked=%v, want an overflow before any claim or call: a summary covers only what its model read", err, stub.calls, q.created, q.markedIDs)
				}
				return
			}
			if err != nil || len(q.markedIDs) != 1 || !strings.Contains(stub.prompt, strings.Repeat("x", tc.tokens*4)) || !strings.Contains(stub.prompt, strings.Repeat("p", tc.earlier*4)) {
				t.Fatalf("err=%v marked=%d, want the whole entry and earlier summaries read in one call", err, len(q.markedIDs))
			}
			if request := contextfrag.ProviderBudgetTokensFromBytes(len(stub.prompt)); request+stub.maxTokens > cfg.SummaryWindowTokens {
				t.Fatalf("request %d + max_tokens %d exceeds the %d window", request, stub.maxTokens, cfg.SummaryWindowTokens)
			}
			if !strings.Contains(stub.prompt, fmt.Sprintf("must stay under %d tokens", stub.maxTokens)) {
				t.Fatalf("stated limit is not the %d tokens the window leaves:\n%s", stub.maxTokens, stub.prompt[len(stub.prompt)-400:])
			}
		})
	}
}

func TestRunCompactionPanicReleasesSlot(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{uncompacted: machineryCorpus(t), listPanic: true}
	stub := &stubModel{summary: "recovered"}
	svc := newMachineryService(q)
	cfg := machineryConfig(stub, 450)

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		_, _ = svc.RunCompactionSync(context.Background(), cfg)
	}()
	if r := <-recovered; r == nil {
		t.Fatal("the panic must propagate, not be swallowed")
	}

	q.listPanic = false
	res, err := svc.RunCompactionSync(context.Background(), cfg)
	if err != nil || res.Status != StatusOK {
		t.Fatalf("the slot must be released after a panic: res=%#v err=%v", res, err)
	}
}

type panickingHookProvider struct {
	armed atomic.Bool
	calls atomic.Int32
}

func (p *panickingHookProvider) MCPClient(context.Context, string) (*bridge.Client, error) {
	p.calls.Add(1)
	if p.armed.Load() {
		panic("boom: injected hook provider panic")
	}
	return nil, errors.New("no workspace client")
}

func TestPreHookPanicReleasesSlot(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{uncompacted: machineryCorpus(t)}
	stub := &stubModel{summary: "unused"}
	svc := newMachineryService(q)
	provider := &panickingHookProvider{}
	provider.armed.Store(true)
	svc.SetHookService(hooks.NewService(slog.New(slog.DiscardHandler), provider))
	cfg := machineryConfig(stub, 450)

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		_, _ = svc.RunCompactionSync(context.Background(), cfg)
	}()
	if r := <-recovered; r == nil {
		t.Fatal("a pre-hook panic must propagate, not be swallowed")
	}

	provider.armed.Store(false)
	before := provider.calls.Load()
	if _, err := svc.RunCompactionSync(context.Background(), cfg); err == nil {
		t.Fatal("the next run must reach the failing pre-hook, proving the slot was released")
	}
	if provider.calls.Load() == before || stub.calls != 0 {
		t.Fatalf("pre-hook calls %d -> %d, model calls %d", before, provider.calls.Load(), stub.calls)
	}
}

// TestPostCompactHookPanicDoesNotPoisonASuccessfulRun pins the post-hook
// panic containment: the compaction itself succeeded and its outcome is
// already persisted, so a panicking PostCompact hook must not escape or turn
// the published result into a lie. The panicking hook service is swapped in
// mid-run, after the pre-hook already passed.
func TestPostCompactHookPanicDoesNotPoisonASuccessfulRun(t *testing.T) {
	t.Parallel()

	q := &fakeQueries{uncompacted: machineryCorpus(t)}
	stub := &stubModel{summary: "SUMMARY-OK"}
	svc := newMachineryService(q)
	provider := &panickingHookProvider{}
	provider.armed.Store(true)
	q.onComplete = func() {
		svc.SetHookService(hooks.NewService(slog.New(slog.DiscardHandler), provider))
	}
	cfg := machineryConfig(stub, 450)

	recovered := make(chan any, 1)
	results := make(chan Result, 1)
	errs := make(chan error, 1)
	go func() {
		defer func() { recovered <- recover() }()
		res, err := svc.RunCompactionSync(context.Background(), cfg)
		results <- res
		errs <- err
	}()
	if r := <-recovered; r != nil {
		t.Fatalf("a post-hook panic must be contained, but escaped: %v", r)
	}
	if err := <-errs; err != nil {
		t.Fatalf("a post-hook panic must not fail the completed compaction: %v", err)
	}
	if res := <-results; res.Status != StatusOK || res.Summary != "SUMMARY-OK" {
		t.Fatalf("completed compaction result lost to a post-hook panic: %#v", res)
	}
	if provider.calls.Load() == 0 {
		t.Fatal("the post-hook must actually have run (and panicked)")
	}

	q.onComplete = nil
	svc.SetHookService(nil)
	if res, err := svc.RunCompactionSync(context.Background(), cfg); err != nil || res.Status != StatusOK {
		t.Fatalf("a contained post-hook panic must leave the session usable: res=%#v err=%v", res, err)
	}
}
