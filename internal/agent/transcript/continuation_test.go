package transcript

import (
	"context"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A run always names its turn, so a handle without one is refused instead of
// answered with an empty request row.
func TestContinuationRequestRefusesAHandleWithoutATurn(t *testing.T) {
	t.Parallel()
	reader := New(Deps{})
	if _, err := reader.ContinuationRequest(t.Context(), "session-1", sessionruntime.RunHandle{RunID: "run-1"}); err == nil {
		t.Fatal("ContinuationRequest() error = nil, want an error for a handle with no turn")
	}
}

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
	reader := New(Deps{Messages: messages})
	got, err := reader.ContinuationRequest(
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
