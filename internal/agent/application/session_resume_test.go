package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
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
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	service := fixture.fixture.service
	setAllowedTeam(service, "00000000-0000-0000-0000-000000000001")
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
	done, err := service.resume.Resume(t.Context(), row)
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
	if _, err := service.resume.Resume(t.Context(), row); !errors.Is(err, runreq.ErrResumeUnrecoverable) {
		t.Fatalf("duplicate admission = %v, want an unrecoverable intent", err)
	}
	expired := time.Now().Add(-time.Second)
	data.DeadlineAt = &expired
	row.InputJson, _ = json.Marshal(map[string]any{"resume": data})
	if done, err := service.resume.Resume(t.Context(), row); !errors.Is(err, runreq.ErrResumeUnrecoverable) || done != nil {
		t.Fatalf("expired budget: done=%v err=%v, want an unrecoverable intent", done, err)
	}
	if len(fixture.admitter.Admitted()) != 2 {
		t.Fatal("expired task reached admission")
	}
}

func TestResumeWaitsForWorkspaceBeforeAdmission(t *testing.T) {
	s, admitter := newAdmittedTurnTestService()
	setAllowedTeam(s, "00000000-0000-0000-0000-000000000001")
	useResumeReadiness(s, func(context.Context, string, string) error { return errors.New("bridge not ready") })
	raw, _ := json.Marshal(map[string]any{"resume": admit.ResumeContext{Version: 1, ChatID: "chat", Query: "work"}})
	done, err := s.resume.Resume(t.Context(), sqlc.SessionRun{TeamID: db.ParseUUIDOrEmpty("00000000-0000-0000-0000-000000000001"), InputJson: raw})
	if err == nil || done != nil {
		t.Fatal("unready workspace admitted")
	}
	if len(admitter.Admitted()) != 0 {
		t.Fatal("readiness failure consumed resume identity")
	}
}

type retireResumeQueries struct {
	dbstore.Queries
	rows    []sqlc.SessionRun
	swept   int
	retired []string
}

func (q *retireResumeQueries) ListInterruptedSessionRuns(context.Context, pgtype.UUID) ([]sqlc.SessionRun, error) {
	rows := q.rows
	q.rows = nil
	return rows, nil
}

func (q *retireResumeQueries) RetireSupersededInterruptedSessionRuns(context.Context) (int64, error) {
	q.swept++
	return 0, nil
}

func (q *retireResumeQueries) RetireInterruptedSessionRun(_ context.Context, runID pgtype.UUID) (int64, error) {
	q.retired = append(q.retired, runID.String())
	return 1, nil
}

// An intent that can never continue is retired once instead of being retried
// on every pass; one that may still clear (the workspace is not up yet) keeps
// its intent, and a resumable one is admitted.
func TestResumeScanRetiresIntentsThatCannotContinue(t *testing.T) {
	const teamID = "00000000-0000-0000-0000-000000000001"
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	s, admitter := fixture.fixture.service, fixture.admitter
	setAllowedTeam(s, teamID)
	unreadyBot := uuid.NewString()
	useResumeReadiness(s, func(_ context.Context, botID, _ string) error {
		if botID == unreadyBot {
			return errors.New("bridge not ready")
		}
		return nil
	})
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
	q := &retireResumeQueries{Queries: s.queries, rows: []sqlc.SessionRun{expired, unsupported, unready, resumable}}
	s.queries = q
	rewireComponents(s)

	scan := resume.Scan{}
	if err := s.resume.ScanInterrupted(t.Context(), &scan, make(chan struct{}, 4)); err != nil {
		t.Fatal(err)
	}
	if q.swept != 1 {
		t.Fatalf("superseded sweep ran %d times, want once per scope pass", q.swept)
	}
	want := []string{expired.RunID.String(), unsupported.RunID.String()}
	if fmt.Sprint(q.retired) != fmt.Sprint(want) {
		t.Fatalf("retired %v, want %v", q.retired, want)
	}
	inputs := admitter.Admitted()
	if len(inputs) != 1 || inputs[0].InvocationID != "resume:"+resumable.RunID.String() {
		t.Fatalf("admission=%+v, want only the resumable run", inputs)
	}
}
