package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

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

func (m *resumeTurnMessages) GetVisibleHistoryTurnRequestMessageIDByTurn(context.Context, string, string) (string, error) {
	return m.requestMessageID, nil
}

// resumeProviderRequests resumes an interrupted run whose request row is in
// history, and returns the requests the model received.
func resumeProviderRequests(t *testing.T, turnID string) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	fixture.service.SetAllowedTeam(driveTeamID)
	history := &resumeTurnMessages{
		driveHistoryMessages: driveHistoryMessages{history: []messagepkg.Message{
			admissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
			admissionHistoryMessage(t, driveReplacedAssistantID, sdk.AssistantMessage("partial work"), nil),
		}},
		requestMessageID: driveRequestMessageID,
	}
	fixture.service.messageService = history
	raw, err := json.Marshal(map[string]any{"resume": resumeContext{
		Version: 1, Query: directLifecyclePrompt, ChatID: lifecycleTestBotID, UserID: "im-user-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	row := sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty(driveTeamID),
		RunID:     db.ParseUUIDOrEmpty(driveInterruptedRunID),
		BotID:     db.ParseUUIDOrEmpty(lifecycleTestBotID),
		SessionID: db.ParseUUIDOrEmpty(lifecycleTestSessionID),
		InputJson: raw,
	}
	if turnID != "" {
		row.TurnID = db.ParseUUIDOrEmpty(turnID)
	}
	done, err := fixture.service.resumeInterruptedSession(context.Background(), row)
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
