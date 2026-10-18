package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/resume"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// A resume re-enters the session it saved, on the request the intent recorded,
// and keeps the deadline the interrupted run was given. The turn resolves
// through the whole application path, so the assertion is on what the model was
// asked for rather than on an intermediate request value.
func TestResumeReentersSavedSessionOnceAndKeepsDeadline(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess,
		withFixtureOptions(withAllowedTeam("00000000-0000-0000-0000-000000000001")))
	service := fixture.fixture.service
	deadline := time.Now().Add(time.Minute)
	data := admit.ResumeContext{Version: 1, ChatID: agenttest.LifecycleBotID, UserID: "sender", Query: "original task", DeadlineAt: &deadline}
	raw, err := json.Marshal(map[string]any{"resume": data})
	if err != nil {
		t.Fatal(err)
	}
	row := sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty("00000000-0000-0000-0000-000000000001"),
		RunID:     db.ParseUUIDOrEmpty(uuid.NewString()),
		BotID:     db.ParseUUIDOrEmpty(agenttest.LifecycleBotID),
		SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID),
		InputJson: raw,
	}
	done, err := startContinuation(t.Context(), service, row)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not finish")
	}
	// The resumed run asks the model the original task, with the instruction
	// that tells it why it is seeing it again.
	requests := fixture.fixture.requests.recorded()
	if len(requests) == 0 {
		t.Fatal("the resumed run never reached the model provider")
	}
	if !strings.Contains(requests[0], "original task") || !strings.Contains(requests[0], resume.Instruction) {
		t.Fatalf("model request lacks the resumed task: %s", requests[0])
	}
	// A resume writes no new user message: the input is synthetic, and the
	// turn it continues is already in history.
	for _, input := range fixture.fixture.messages.Persisted {
		if input.Role == "user" {
			t.Fatalf("resume persisted a user message: %+v", input)
		}
	}

	inputs := fixture.admitter.Admitted()
	if len(inputs) != 1 || inputs[0].InvocationID != "resume:"+row.RunID.String() {
		t.Fatalf("admission=%+v", inputs)
	}
	fixture.admitter.Mu.Lock()
	fixture.admitter.Started = false
	fixture.admitter.Mu.Unlock()
	if _, err := startContinuation(t.Context(), service, row); !errors.Is(err, runreq.ErrResumeUnrecoverable) {
		t.Fatalf("duplicate admission = %v, want an unrecoverable intent", err)
	}
	expired := time.Now().Add(-time.Second)
	data.DeadlineAt = &expired
	row.InputJson, _ = json.Marshal(map[string]any{"resume": data})
	if done, err := startContinuation(t.Context(), service, row); !errors.Is(err, runreq.ErrResumeUnrecoverable) || done != nil {
		t.Fatalf("expired budget: done=%v err=%v, want an unrecoverable intent", done, err)
	}
	if len(fixture.admitter.Admitted()) != 2 {
		t.Fatal("expired task reached admission")
	}
}

// startContinuation starts the continuation of row the way pending does once it
// has decided the session's turn has come.
func startContinuation(ctx context.Context, s *Service, row sqlc.SessionRun) (<-chan struct{}, error) {
	return s.continuer.Start(ctx, pending.Continuation{Run: row})
}

// resumeIntentQueries serves the interrupted runs a test lists, one per
// session, and records the intents retired; a retired run is owed no more.
type resumeIntentQueries struct {
	dbstore.Queries
	mu      sync.Mutex
	owed    map[string]sqlc.SessionRun
	swept   int
	retired []string
}

func newResumeIntentQueries(rows ...sqlc.SessionRun) *resumeIntentQueries {
	q := &resumeIntentQueries{owed: map[string]sqlc.SessionRun{}}
	for _, row := range rows {
		q.owed[row.SessionID.String()] = row
	}
	return q
}

func (q *resumeIntentQueries) ListInterruptedSessionRuns(context.Context, pgtype.UUID) ([]sqlc.SessionRun, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var rows []sqlc.SessionRun
	for _, row := range q.owed {
		rows = append(rows, row)
	}
	return rows, nil
}

func (q *resumeIntentQueries) GetSessionInterruptedRun(_ context.Context, sessionID pgtype.UUID) (sqlc.SessionRun, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	row, ok := q.owed[sessionID.String()]
	if !ok {
		return sqlc.SessionRun{}, pgx.ErrNoRows
	}
	return row, nil
}

func (q *resumeIntentQueries) RetireSupersededInterruptedSessionRuns(context.Context) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.swept++
	return 0, nil
}

func (q *resumeIntentQueries) RetireInterruptedSessionRun(_ context.Context, runID pgtype.UUID) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for session, row := range q.owed {
		if row.RunID == runID {
			delete(q.owed, session)
		}
	}
	q.retired = append(q.retired, runID.String())
	return 1, nil
}

func (q *resumeIntentQueries) retiredRuns() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Sorted(slices.Values(q.retired))
}

// A scan wakes pending for every session that is owed a continuation, and
// pending decides each one: an intent that can never continue is retired once
// instead of being retried on every pass; one that may still clear (the
// workspace is not up yet) keeps its intent, and a resumable one is admitted.
func TestResumeScanWakesPendingAndOnlyUnrecoverableIntentsAreRetired(t *testing.T) {
	const teamID = "00000000-0000-0000-0000-000000000001"
	unreadyBot := uuid.NewString()
	future, past := time.Now().Add(time.Minute), time.Now().Add(-time.Second)
	row := func(botID string, data admit.ResumeContext) sqlc.SessionRun {
		raw, err := json.Marshal(map[string]any{"resume": data})
		if err != nil {
			t.Fatal(err)
		}
		return sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty(teamID), RunID: db.ParseUUIDOrEmpty(uuid.NewString()), BotID: db.ParseUUIDOrEmpty(botID), SessionID: db.ParseUUIDOrEmpty(uuid.NewString()), InputJson: raw}
	}
	expired := row(uuid.NewString(), admit.ResumeContext{Version: 1, ChatID: agenttest.LifecycleBotID, Query: "late", DeadlineAt: &past})
	unsupported := row(uuid.NewString(), admit.ResumeContext{Version: 2, ChatID: agenttest.LifecycleBotID, Query: "future format"})
	unready := row(unreadyBot, admit.ResumeContext{Version: 1, ChatID: agenttest.LifecycleBotID, Query: "wait", DeadlineAt: &future})
	resumable := row(uuid.NewString(), admit.ResumeContext{Version: 1, ChatID: agenttest.LifecycleBotID, Query: "go on", DeadlineAt: &future})
	// The resumable row runs the whole turn, so the store the fixture serves
	// stays underneath the resume-specific reads this test added.
	q := newResumeIntentQueries(expired, unsupported, unready, resumable)
	var unreadyChecks atomic.Int32
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess,
		withFixtureOptions(withAllowedTeam(teamID), withWorkspaceReadiness(func(_ context.Context, botID, _ string) error {
			if botID == unreadyBot {
				unreadyChecks.Add(1)
				return errors.New("bridge not ready")
			}
			return nil
		})),
		withFixtureQueries(func(base dbstore.Queries) dbstore.Queries { q.Queries = base; return q }))
	s, admitter := fixture.fixture.service, fixture.admitter

	scan := resume.Scan{}
	if err := s.resume.ScanInterrupted(t.Context(), &scan); err != nil {
		t.Fatal(err)
	}
	// The wakes run beside the scan: wait for each session's decision.
	require.Eventually(t, func() bool {
		return len(q.retiredRuns()) == 2 && len(admitter.Admitted()) == 1 && unreadyChecks.Load() > 0
	}, 10*time.Second, 5*time.Millisecond, "retired=%v admitted=%d unreadyChecks=%d", q.retiredRuns(), len(admitter.Admitted()), unreadyChecks.Load())
	q.mu.Lock()
	swept := q.swept
	q.mu.Unlock()
	if swept != 1 {
		t.Fatalf("superseded sweep ran %d times, want once per scope pass", swept)
	}
	want := []string{expired.RunID.String(), unsupported.RunID.String()}
	slices.Sort(want)
	if fmt.Sprint(q.retiredRuns()) != fmt.Sprint(want) {
		t.Fatalf("retired %v, want %v", q.retiredRuns(), want)
	}
	inputs := admitter.Admitted()
	if len(inputs) != 1 || inputs[0].InvocationID != "resume:"+resumable.RunID.String() {
		t.Fatalf("admission=%+v, want only the resumable run", inputs)
	}
}
