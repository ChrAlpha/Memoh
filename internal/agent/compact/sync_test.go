package compact

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func TestCompactionNeed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		pressure  int
		budget    int
		threshold int
		want      urgency
	}{
		{"below the soft share", 99999, 200000, 0, urgencyNone},
		{"at the soft share", 100000, 200000, 0, urgencyAsync},
		{"below the hard share", 149999, 200000, 0, urgencyAsync},
		{"at the hard share", 150000, 200000, 0, urgencySync},
		{"above the hard share", 190000, 200000, 0, urgencySync},
		{"configured threshold lowers the async trigger", 90000, 200000, 90000, urgencyAsync},
		{"configured threshold never moves the hard share", 149999, 200000, 500000, urgencyNone},
		{"configured threshold above the hard share still syncs there", 150000, 200000, 500000, urgencySync},
		{"no window means no compaction", 1000000, 0, 0, urgencyNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := urgencyOf(tc.pressure, tc.budget, tc.threshold); got != tc.want {
				t.Fatalf("urgencyOf(%d, %d, %d) = %d, want %d", tc.pressure, tc.budget, tc.threshold, got, tc.want)
			}
		})
	}
}

// scriptedCompactionRunner answers a compaction request with a fixed outcome
// and lets a test change the world the way a finished compaction would.
type scriptedCompactionRunner struct {
	mu     sync.Mutex
	status string
	err    error
	onRun  func()
	calls  int
}

func (r *scriptedCompactionRunner) RunCompactionSync(context.Context, compaction.TriggerConfig) (compaction.Result, error) {
	r.mu.Lock()
	r.calls++
	status, err, onRun := r.status, r.err, r.onRun
	r.mu.Unlock()
	if onRun != nil {
		onRun()
	}
	return compaction.Result{Status: status}, err
}

func (r *scriptedCompactionRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// stagedHistory serves one set of rows until a compaction "finishes", then
// another.
type stagedHistory struct {
	agenttest.AssembleMessages
	mu        sync.Mutex
	compacted bool
	big       []messagepkg.Message
	small     []messagepkg.Message
}

func (s *stagedHistory) markCompacted() {
	s.mu.Lock()
	s.compacted = true
	s.mu.Unlock()
}

func (s *stagedHistory) ListActiveAfterCompactionBySessionWithinBytes(ctx context.Context, sessionID string, maxBytes int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	s.mu.Lock()
	rows := s.big
	if s.compacted {
		rows = s.small
	}
	s.mu.Unlock()
	s.After = rows
	return s.AssembleMessages.ListActiveAfterCompactionBySessionWithinBytes(ctx, sessionID, maxBytes)
}

// noFrontierQueries is the controller store for a session with no compaction
// artifacts.
type noFrontierQueries struct{ *controllerQueries }

func (noFrontierQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

// GetLatestContextLifecycleBySession reports a session whose earlier runs
// recorded no context plan.
func (noFrontierQueries) GetLatestContextLifecycleBySession(context.Context, pgtype.UUID) ([]byte, error) {
	return nil, pgx.ErrNoRows
}

// lastRunPlanQueries is a session whose latest run spent fixed tokens on its
// system prompt and tools.
type lastRunPlanQueries struct {
	noFrontierQueries
	system, tools int
	reads         *int
}

func (q lastRunPlanQueries) GetLatestContextLifecycleBySession(context.Context, pgtype.UUID) ([]byte, error) {
	*q.reads++
	return json.Marshal(contextfrag.LifecycleSnapshot{
		Version:    1,
		BudgetPlan: &contextfrag.ContextBudgetPlan{Window: syncTestBudget, ActualSystemCost: q.system, ToolDefsCost: q.tools},
	})
}

func syncTestEstimate(service *fixture, req runreq.ChatRequest) *Estimate {
	return service.NewEstimate(context.Background(), req.ThreadID, nil)
}

const syncTestBudget = 1000

func newSyncCompactionFixture(t *testing.T, status string) (*fixture, *scriptedCompactionRunner, *stagedHistory, runreq.ChatRequest) {
	t.Helper()
	service, _ := newControllerPolicyService(t, nil)
	service.queries = noFrontierQueries{service.queries.(*controllerQueries)}
	service.rewire()
	runner := &scriptedCompactionRunner{status: status}
	history := &stagedHistory{
		big:   []messagepkg.Message{agenttest.AssembleText(t, "big", "user", strings.Repeat("x", 4000), agenttest.AssembleBase)},
		small: []messagepkg.Message{agenttest.AssembleText(t, "small", "user", "<summary>short</summary>", agenttest.AssembleBase)},
	}
	service.engine = runner
	service.messages = history
	service.rewire()
	req := runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
		RunID:    "run-1",
	}
	return service, runner, history, req
}

func assembleForSyncTest(t *testing.T, service *fixture, req runreq.ChatRequest) transcript.Assembled {
	t.Helper()
	assembled, err := service.transcript.Assemble(context.Background(), transcript.ForChat(transcript.PurposeRun, req, syncTestBudget))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	return assembled
}

func recordTokens(assembled transcript.Assembled) int { return assembled.CompactableTokens }

func TestCompactBeforeTurnCompactsOnceAtTheHardShareAndReassembles(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	before := assembleForSyncTest(t, service, req)
	if got := urgencyOf(recordTokens(before), syncTestBudget, 0); got != urgencySync {
		t.Fatalf("fixture pressure %d does not reach the hard share of %d", recordTokens(before), syncTestBudget)
	}

	after, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, syncTestEstimate(service, req))
	if err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once", runner.callCount())
	}
	agenttest.AssertTexts(t, after.Messages, "<summary>short</summary>")
}

func TestCompactBeforeTurnLeavesContextBelowTheHardShareAlone(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	history.big = history.small
	before := assembleForSyncTest(t, service, req)
	loads := history.AfterCalls

	after, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, syncTestEstimate(service, req))
	if err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if runner.callCount() != 0 || history.AfterCalls != loads || len(after.Messages) != len(before.Messages) {
		t.Fatalf("calls=%d loads=%d->%d: a context below the hard share must be left alone", runner.callCount(), loads, history.AfterCalls)
	}
}

// The turn judges the whole request it is about to send. Rows well under the
// hard share still call for compaction when the system prompt and tools the
// session's last run measured, and the user's message, carry it over.
func TestCompactBeforeTurnCountsTheWholeRequest(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	history.big = []messagepkg.Message{agenttest.AssembleText(t, "rows", "user", strings.Repeat("r", 600), agenttest.AssembleBase)}
	runner.onRun = history.markCompacted
	reads := 0
	service.queries = lastRunPlanQueries{noFrontierQueries: service.queries.(noFrontierQueries), system: 400, tools: 250, reads: &reads}
	service.rewire()
	before := assembleForSyncTest(t, service, req)
	if got := urgencyOf(recordTokens(before), syncTestBudget, 0); got == urgencySync {
		t.Fatalf("fixture rows alone (%d) already reach the hard share", recordTokens(before))
	}
	estimate := syncTestEstimate(service, req)
	estimate.own = UserMessageEstimate(runreq.ChatRequest{}, "what now?")

	after, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, estimate)
	if err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once: rows %d plus the fixed part cross the hard share", runner.callCount(), recordTokens(before))
	}
	agenttest.AssertTexts(t, after.Messages, "<summary>short</summary>")
}

// Compaction can only take raw rows. A request over the hard share without
// any (a first turn, or a session already reduced to summaries) is sent as
// it is, without a compaction call or a lifecycle read.
func TestCompactBeforeTurnWithoutRawRowsSendsTheRequestAsIs(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	history.big = nil
	reads := 0
	service.queries = lastRunPlanQueries{noFrontierQueries: service.queries.(noFrontierQueries), system: 900, tools: 100, reads: &reads}
	service.rewire()
	before := assembleForSyncTest(t, service, req)

	if _, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, syncTestEstimate(service, req)); err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if runner.callCount() != 0 || reads != 0 {
		t.Fatalf("compaction calls=%d lifecycle reads=%d, want none without raw rows", runner.callCount(), reads)
	}
}

// Only a pass that produced a summary changes what the turn sends; failure,
// cooldown and an empty pass all send the context as assembled.
func TestCompactBeforeTurnSendsTheContextAsIsUnlessCompactionSucceeded(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status string
		err    error
	}{
		{"nothing to compact or cooldown", compaction.StatusNoop, nil},
		{"failure", "", errors.New("summarizer unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service, runner, history, req := newSyncCompactionFixture(t, tc.status)
			runner.err = tc.err
			// The rows would shrink if the turn reassembled after a failed pass.
			runner.onRun = history.markCompacted
			before := assembleForSyncTest(t, service, req)
			loads := history.AfterCalls

			after, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, syncTestEstimate(service, req))
			if err != nil {
				t.Fatalf("BeforeTurn: %v", err)
			}
			if runner.callCount() != 1 {
				t.Fatalf("compaction ran %d times, want once", runner.callCount())
			}
			if history.AfterCalls != loads {
				t.Fatalf("history was reassembled after a pass that produced no summary")
			}
			if len(after.Messages) != 1 || strings.Contains(after.Messages[0].TextContent(), "summary") {
				t.Fatalf("messages = %q, want the context as assembled", agenttest.MessageTexts(after.Messages))
			}
		})
	}
}

// A load that stopped at its byte budget is compacted even when the rows it
// did load are under the hard share.
func TestCompactBeforeTurnCompactsATruncatedLoad(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	history.big = []messagepkg.Message{agenttest.AssembleText(t, "tail", "user", "short tail", agenttest.AssembleBase)}
	runner.onRun = history.markCompacted
	before := assembleForSyncTest(t, service, req)
	before.Truncated = true

	after, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, syncTestEstimate(service, req))
	if err != nil {
		t.Fatalf("BeforeTurn: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once for a truncated load", runner.callCount())
	}
	agenttest.AssertTexts(t, after.Messages, "<summary>short</summary>")
}

// A compaction already running is waited for. Its outcome is read from the
// store and the context judged again; when it already made room, this turn
// does not compact a second time.
func TestCompactBeforeTurnWaitsForARunningCompactionAndJudgesAgain(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	before := assembleForSyncTest(t, service, req)

	running := service.enter(req.BotID, req.ThreadID)
	type outcome struct {
		assembled transcript.Assembled
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		assembled, err := service.BeforeTurn(context.Background(), req, "", syncTestBudget, before, syncTestEstimate(service, req))
		done <- outcome{assembled, err}
	}()

	select {
	case <-done:
		t.Fatal("the turn went on while a compaction held the session")
	case <-time.After(50 * time.Millisecond):
	}
	history.markCompacted()
	running()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("BeforeTurn: %v", got.err)
		}
		agenttest.AssertTexts(t, got.assembled.Messages, "<summary>short</summary>")
	case <-time.After(2 * time.Second):
		t.Fatal("the turn did not resume after the compaction finished")
	}
	if runner.callCount() != 0 {
		t.Fatalf("compaction ran %d times, want none: the one in flight had made room", runner.callCount())
	}
}

// Waiting for a compaction does not deadlock against the run's own reader on
// the barrier, even with a compaction queued behind it.
func TestAwaitSessionCompactionStepsAsideForTheRunsOwnReader(t *testing.T) {
	t.Parallel()

	service := New(Deps{})
	releaseRun := service.Defer("bot-1", "session-1", "run-1")
	defer releaseRun()
	queued := make(chan struct{})
	go func() {
		done := service.enter("bot-1", "session-1")
		close(queued)
		done()
	}()
	assertChannelBlocked(t, queued, "a compaction started while the run still held its reader")

	waited := make(chan bool, 1)
	go func() { waited <- service.await("bot-1", "session-1", "run-1") }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("awaiting a compaction deadlocked against the run's own reader")
	}
	assertChannelReady(t, queued, "the queued compaction never ran")
}

func TestAwaitSessionCompactionReportsWhetherItWaited(t *testing.T) {
	t.Parallel()

	service := New(Deps{})
	if service.await("bot-1", "session-1", "run-1") {
		t.Fatal("reported a wait on an idle session")
	}
	running := service.enter("bot-1", "session-1")
	waited := make(chan bool, 1)
	go func() { waited <- service.await("bot-1", "session-1", "run-1") }()
	assertChannelBlocked(t, waited, "returned while a compaction held the session")
	running()
	select {
	case got := <-waited:
		if !got {
			t.Fatal("did not report the wait")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never resumed")
	}
}

// A turn cancelled while its context is being compacted ends there. Sending the
// uncompacted context on would start a model call for a turn nobody wants.
func TestCompactBeforeTurnCancelledDuringCompactionEndsTheTurn(t *testing.T) {
	t.Parallel()

	service, runner, _, req := newSyncCompactionFixture(t, "")
	stop := errors.New("stopped by the user")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	runner.err = context.Canceled
	runner.onRun = func() { cancel(stop) }
	before := assembleForSyncTest(t, service, req)

	_, err := service.BeforeTurn(ctx, req, "", syncTestBudget, before, syncTestEstimate(service, req))

	if !errors.Is(err, stop) {
		t.Fatalf("BeforeTurn error = %v, want the cancellation's cause", err)
	}
}
