package application

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
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
		want      compactionLevel
	}{
		{"below the soft share", 99999, 200000, 0, compactionNone},
		{"at the soft share", 100000, 200000, 0, compactionAsync},
		{"below the hard share", 149999, 200000, 0, compactionAsync},
		{"at the hard share", 150000, 200000, 0, compactionSync},
		{"above the hard share", 190000, 200000, 0, compactionSync},
		{"configured threshold lowers the async trigger", 90000, 200000, 90000, compactionAsync},
		{"configured threshold never moves the hard share", 149999, 200000, 500000, compactionNone},
		{"configured threshold above the hard share still syncs there", 150000, 200000, 500000, compactionSync},
		{"no window means no compaction", 1000000, 0, 0, compactionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := compactionNeed(tc.pressure, tc.budget, tc.threshold); got != tc.want {
				t.Fatalf("compactionNeed(%d, %d, %d) = %d, want %d", tc.pressure, tc.budget, tc.threshold, got, tc.want)
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
	assembleMessageService
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
	s.after = rows
	return s.assembleMessageService.ListActiveAfterCompactionBySessionWithinBytes(ctx, sessionID, maxBytes)
}

// noFrontierQueries is the controller store for a session with no compaction
// artifacts.
type noFrontierQueries struct{ *controllerQueries }

func (noFrontierQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

const syncTestBudget = 1000

func newSyncCompactionFixture(t *testing.T, status string) (*Service, *scriptedCompactionRunner, *stagedHistory, ChatRequest) {
	t.Helper()
	service, _ := newControllerPolicyService(t, nil)
	service.queries = noFrontierQueries{service.queries.(*controllerQueries)}
	runner := &scriptedCompactionRunner{status: status}
	history := &stagedHistory{
		big:   []messagepkg.Message{assembleText(t, "big", "user", strings.Repeat("x", 4000), assembleBase)},
		small: []messagepkg.Message{assembleText(t, "small", "user", "<summary>short</summary>", assembleBase)},
	}
	service.compactionService = runner
	service.messageService = history
	req := ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
		RunID:    "run-1",
	}
	return service, runner, history, req
}

func assembleForSyncTest(t *testing.T, service *Service, req ChatRequest) assembledContext {
	t.Helper()
	assembled, err := service.assembleContext(context.Background(), chatContextRequest(contextPurposeRun, req, syncTestBudget))
	if err != nil {
		t.Fatalf("assembleContext: %v", err)
	}
	return assembled
}

func recordTokens(assembled assembledContext) int { return assembled.CompactableTokens }

func TestCompactBeforeTurnCompactsOnceAtTheHardShareAndReassembles(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	before := assembleForSyncTest(t, service, req)
	if got := compactionNeed(recordTokens(before), syncTestBudget, 0); got != compactionSync {
		t.Fatalf("fixture pressure %d does not reach the hard share of %d", recordTokens(before), syncTestBudget)
	}

	after, err := service.compactBeforeTurn(context.Background(), req, "", syncTestBudget, before)
	if err != nil {
		t.Fatalf("compactBeforeTurn: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once", runner.callCount())
	}
	assertTexts(t, after.Messages, "<summary>short</summary>")
}

func TestCompactBeforeTurnLeavesContextBelowTheHardShareAlone(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	history.big = history.small
	before := assembleForSyncTest(t, service, req)
	loads := history.afterCalls

	after, err := service.compactBeforeTurn(context.Background(), req, "", syncTestBudget, before)
	if err != nil {
		t.Fatalf("compactBeforeTurn: %v", err)
	}
	if runner.callCount() != 0 || history.afterCalls != loads || len(after.Messages) != len(before.Messages) {
		t.Fatalf("calls=%d loads=%d->%d: a context below the hard share must be left alone", runner.callCount(), loads, history.afterCalls)
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
			loads := history.afterCalls

			after, err := service.compactBeforeTurn(context.Background(), req, "", syncTestBudget, before)
			if err != nil {
				t.Fatalf("compactBeforeTurn: %v", err)
			}
			if runner.callCount() != 1 {
				t.Fatalf("compaction ran %d times, want once", runner.callCount())
			}
			if history.afterCalls != loads {
				t.Fatalf("history was reassembled after a pass that produced no summary")
			}
			if len(after.Messages) != 1 || strings.Contains(after.Messages[0].TextContent(), "summary") {
				t.Fatalf("messages = %q, want the context as assembled", messageTexts(after.Messages))
			}
		})
	}
}

// A load that stopped at its byte budget is compacted even when the rows it
// did load are under the hard share.
func TestCompactBeforeTurnCompactsATruncatedLoad(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	history.big = []messagepkg.Message{assembleText(t, "tail", "user", "short tail", assembleBase)}
	runner.onRun = history.markCompacted
	before := assembleForSyncTest(t, service, req)
	before.Truncated = true

	after, err := service.compactBeforeTurn(context.Background(), req, "", syncTestBudget, before)
	if err != nil {
		t.Fatalf("compactBeforeTurn: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once for a truncated load", runner.callCount())
	}
	assertTexts(t, after.Messages, "<summary>short</summary>")
}

// A compaction already running is waited for. Its outcome is read from the
// store and the context judged again; when it already made room, this turn
// does not compact a second time.
func TestCompactBeforeTurnWaitsForARunningCompactionAndJudgesAgain(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	before := assembleForSyncTest(t, service, req)

	running := service.enterSessionCompaction(req.BotID, req.ThreadID)
	type outcome struct {
		assembled assembledContext
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		assembled, err := service.compactBeforeTurn(context.Background(), req, "", syncTestBudget, before)
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
			t.Fatalf("compactBeforeTurn: %v", got.err)
		}
		assertTexts(t, got.assembled.Messages, "<summary>short</summary>")
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

	service := &Service{}
	releaseRun := service.DeferSessionCompaction("bot-1", "session-1", "run-1")
	defer releaseRun()
	queued := make(chan struct{})
	go func() {
		done := service.enterSessionCompaction("bot-1", "session-1")
		close(queued)
		done()
	}()
	assertChannelBlocked(t, queued, "a compaction started while the run still held its reader")

	waited := make(chan bool, 1)
	go func() { waited <- service.awaitSessionCompaction("bot-1", "session-1", "run-1") }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("awaiting a compaction deadlocked against the run's own reader")
	}
	assertChannelReady(t, queued, "the queued compaction never ran")
}

func TestAwaitSessionCompactionReportsWhetherItWaited(t *testing.T) {
	t.Parallel()

	service := &Service{}
	if service.awaitSessionCompaction("bot-1", "session-1", "run-1") {
		t.Fatal("reported a wait on an idle session")
	}
	running := service.enterSessionCompaction("bot-1", "session-1")
	waited := make(chan bool, 1)
	go func() { waited <- service.awaitSessionCompaction("bot-1", "session-1", "run-1") }()
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
