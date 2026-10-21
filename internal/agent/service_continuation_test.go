package agent

import (
	"context"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type continuationTurnRequestResolver struct {
	messagepkg.Service
	requestMessageID string
	sessionID        string
	runID            string
	rootTurnID       string
}

func (s *continuationTurnRequestResolver) GetRunTurnRequestMessageID(_ context.Context, sessionID, runID, rootTurnID string) (string, error) {
	s.sessionID, s.runID, s.rootTurnID = sessionID, runID, rootTurnID
	return s.requestMessageID, nil
}

func TestContinuationTurnRequestMessageIDUsesAdmittedTurn(t *testing.T) {
	t.Parallel()

	messages := &continuationTurnRequestResolver{
		requestMessageID: "request-message-id",
	}
	service := newTestService(t, Deps{Messages: messages})
	got, err := service.transcript.ContinuationRequestMessageID(
		context.Background(),
		"session-id",
		sessionruntime.RunHandle{RunID: "run-id", TurnID: "turn-id"},
	)
	if err != nil {
		t.Fatalf("continuationTurnRequestMessageID() error = %v", err)
	}
	if got != "request-message-id" {
		t.Fatalf("continuation request message ID = %q, want %q", got, "request-message-id")
	}
	if messages.sessionID != "session-id" || messages.runID != "run-id" || messages.rootTurnID != "turn-id" {
		t.Fatalf("continuation lookup omitted run/turn identity: %#v", messages)
	}
}
