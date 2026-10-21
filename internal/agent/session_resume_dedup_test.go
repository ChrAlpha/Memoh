package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
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

func (m *resumeTurnMessages) GetRunTurnRequestMessageID(context.Context, string, string, string) (string, error) {
	return m.requestMessageID, nil
}

// resumeProviderRequests resumes an interrupted run whose request row is in
// history, and returns the requests the model received.
func resumeProviderRequests(t *testing.T, turnID string) []sdk.Request {
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
		InputJson: raw,
	}
	if turnID != "" {
		row.TurnID = db.ParseUUIDOrEmpty(turnID)
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
	requests := resumeProviderRequests(t, driveReplacedTurnID)
	if got := countPromptInRequests(t, requests); got != 1 {
		t.Fatalf("original task appears %d times in the provider request, want 1", got)
	}
}

// A record without a turn (written before the turn was tracked) has no row to
// name; resume still runs and keeps both copies.
func TestResumeWithoutTurnKeepsHistoryCopy(t *testing.T) {
	requests := resumeProviderRequests(t, "")
	if got := countPromptInRequests(t, requests); got != 2 {
		t.Fatalf("original task appears %d times, want 2 without a resolvable turn", got)
	}
}
