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

func (s *continuationTurnRequestResolver) GetRunTurnRequest(_ context.Context, sessionID, runID, rootTurnID string) (messagepkg.RunTurnRequest, error) {
	s.sessionID, s.runID, s.rootTurnID = sessionID, runID, rootTurnID
	return messagepkg.RunTurnRequest{MessageID: s.requestMessageID, TurnID: "request-turn-id", TurnPosition: 7}, nil
}

func TestContinuationRequestUsesAdmittedTurn(t *testing.T) {
	t.Parallel()

	messages := &continuationTurnRequestResolver{
		requestMessageID: "request-message-id",
	}
	service := newTestService(t, Deps{Messages: messages})
	got, err := service.transcript.ContinuationRequest(
		context.Background(),
		"session-id",
		sessionruntime.RunHandle{RunID: "run-id", TurnID: "turn-id"},
	)
	if err != nil {
		t.Fatalf("ContinuationRequest() error = %v", err)
	}
	if got.MessageID != "request-message-id" || got.TurnID != "request-turn-id" || got.TurnPosition != 7 {
		t.Fatalf("continuation request = %#v, want the resolver's row with its turn", got)
	}
	if messages.sessionID != "session-id" || messages.runID != "run-id" || messages.rootTurnID != "turn-id" {
		t.Fatalf("continuation lookup omitted run/turn identity: %#v", messages)
	}
}
