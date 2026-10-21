package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// resumeTurnMessages serves the history of an interrupted turn and resolves
// the turn's request row the way the PostgreSQL store does.
type resumeTurnMessages struct {
	driveHistoryMessages
	requestMessageID string
}

func (m *resumeTurnMessages) GetRunTurnRequest(_ context.Context, _, _, rootTurnID string) (messagepkg.RunTurnRequest, error) {
	return messagepkg.RunTurnRequest{MessageID: m.requestMessageID, TurnID: rootTurnID}, nil
}

// resumeProviderRequests resumes an interrupted run whose request row is in
// history, and returns the requests the model received.
func resumeProviderRequests(t *testing.T) []sdk.Request {
	t.Helper()
	history := &resumeTurnMessages{
		driveHistoryMessages: driveHistoryMessages{history: []messagepkg.Message{
			agenttest.AdmissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
			agenttest.AdmissionHistoryMessage(t, driveReplacedAssistantID, sdk.AssistantMessage("partial work"), nil),
		}},
		requestMessageID: driveRequestMessageID,
	}
	fixture, recorder := newDriveRequestFixture(t, withFixtureOptions(withAllowedTeam(driveTeamID)), withFixtureMessages(history))
	raw, err := json.Marshal(map[string]any{"resume": admit.ResumeContext{
		Version: 1, Query: directLifecyclePrompt, ChatID: agenttest.LifecycleBotID, UserID: "im-user-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	row := sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty(driveTeamID),
		RunID:     db.ParseUUIDOrEmpty(driveInterruptedRunID),
		BotID:     db.ParseUUIDOrEmpty(agenttest.LifecycleBotID),
		SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID),
		TurnID:    db.ParseUUIDOrEmpty(driveReplacedTurnID),
		InputJson: raw,
	}
	done, err := startContinuation(context.Background(), fixture.service, row)
	if err != nil {
		t.Fatalf("resumeInterruptedSession() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resumed run did not finish")
	}
	return requireDriveRequests(t, recorder)
}

func countPromptInRequests(t *testing.T, requests []sdk.Request) int {
	t.Helper()
	raw, err := json.Marshal(requests)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), directLifecyclePrompt)
}

// The resume instruction carries the original task, so the history copy of the
// request row is dropped when the interrupted turn's row can be named.
func TestResumeSendsTheOriginalTaskToTheModelOnce(t *testing.T) {
	requests := resumeProviderRequests(t)
	if got := countPromptInRequests(t, requests); got != 1 {
		t.Fatalf("original task appears %d times in the provider request, want 1", got)
	}
}

// flakyRunRequest resolves the interrupted run's request row through a lookup
// that can fail or find nothing.
type flakyRunRequest struct {
	resumeTurnMessages
	err error
}

func (m *flakyRunRequest) GetRunTurnRequest(ctx context.Context, sessionID, runID, rootTurnID string) (messagepkg.RunTurnRequest, error) {
	if m.err != nil {
		return messagepkg.RunTurnRequest{}, m.err
	}
	return m.resumeTurnMessages.GetRunTurnRequest(ctx, sessionID, runID, rootTurnID)
}

func interruptedRow(t *testing.T) sqlc.SessionRun {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"resume": admit.ResumeContext{
		Version: 1, Query: directLifecyclePrompt, ChatID: agenttest.LifecycleBotID, UserID: "im-user-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty(driveTeamID),
		RunID:     db.ParseUUIDOrEmpty(driveInterruptedRunID),
		BotID:     db.ParseUUIDOrEmpty(agenttest.LifecycleBotID),
		SessionID: db.ParseUUIDOrEmpty(agenttest.LifecycleSessionID),
		TurnID:    db.ParseUUIDOrEmpty(driveReplacedTurnID),
		InputJson: raw,
	}
}

// A lookup that fails says nothing about the row, so the continuation is not
// started: the error returns for pending to keep the intent and try again, and
// a later start with the lookup back joins the row's turn.
func TestResumeRequestLookupFailureStartsNothingAndRecovers(t *testing.T) {
	lookup := &flakyRunRequest{
		resumeTurnMessages: resumeTurnMessages{
			driveHistoryMessages: driveHistoryMessages{history: []messagepkg.Message{
				agenttest.AdmissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
			}},
			requestMessageID: driveRequestMessageID,
		},
		err: errors.New("database unavailable"),
	}
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess, withFixtureOptions(withAllowedTeam(driveTeamID)), withFixtureMessages(lookup))
	row := interruptedRow(t)
	done, err := startContinuation(context.Background(), fixture.fixture.service, row)
	if err == nil || errors.Is(err, runreq.ErrResumeUnrecoverable) || done != nil {
		t.Fatalf("lookup failure: done=%v err=%v, want the lookup error and no run", done, err)
	}
	if got := len(fixture.admitter.Admitted()); got != 0 {
		t.Fatalf("a failed lookup reached admission %d times", got)
	}
	lookup.err = nil
	done, err = startContinuation(context.Background(), fixture.fixture.service, row)
	if err != nil {
		t.Fatalf("start after the lookup recovered: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resumed run did not finish")
	}
	admitted := fixture.admitter.Admitted()
	if len(admitted) != 1 || admitted[0].JoinTurn == nil || admitted[0].JoinTurn.ID != driveReplacedTurnID {
		t.Fatalf("admission = %+v, want one joining the request row's turn", admitted)
	}
}

// A run with no request row has nothing to answer: the intent is unrecoverable
// and nothing is admitted or written.
func TestResumeWithoutRequestRowIsUnrecoverable(t *testing.T) {
	lookup := &flakyRunRequest{err: messagepkg.ErrRunRequestNotFound}
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess, withFixtureOptions(withAllowedTeam(driveTeamID)), withFixtureMessages(lookup))
	done, err := startContinuation(context.Background(), fixture.fixture.service, interruptedRow(t))
	if !errors.Is(err, runreq.ErrResumeUnrecoverable) || !errors.Is(err, messagepkg.ErrRunRequestNotFound) || done != nil {
		t.Fatalf("done=%v err=%v, want an unrecoverable intent", done, err)
	}
	if got := len(fixture.admitter.Admitted()); got != 0 {
		t.Fatalf("a run without a request row reached admission %d times", got)
	}
}
