package application

import (
	"context"
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type continuationTurnRequestResolverService struct {
	messagepkg.Service
	requestMessageID string
}

func (s *continuationTurnRequestResolverService) GetVisibleHistoryTurnRequestMessageIDByTurn(context.Context, string, string) (string, error) {
	return s.requestMessageID, nil
}

func TestContinuationTurnRequestMessageIDUsesAdmittedTurn(t *testing.T) {
	t.Parallel()

	service := wireComponents(&Service{messageService: &continuationTurnRequestResolverService{
		requestMessageID: "request-message-id",
	}})
	got, err := service.transcript.ContinuationRequestMessageID(
		context.Background(),
		"session-id",
		sessionruntime.RunHandle{TurnID: "turn-id"},
	)
	if err != nil {
		t.Fatalf("continuationTurnRequestMessageID() error = %v", err)
	}
	if got != "request-message-id" {
		t.Fatalf("continuation request message ID = %q, want %q", got, "request-message-id")
	}
}
