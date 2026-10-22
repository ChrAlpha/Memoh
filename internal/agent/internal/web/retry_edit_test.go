package web

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type replacementOperationMessageService struct {
	agenttest.RecordingMessages
	latest      messagepkg.HistoryTurn
	latestErr   error
	turnByMsg   messagepkg.HistoryTurn
	turnByMsgID string
}

func (s *replacementOperationMessageService) GetLatestVisibleTurnBySession(context.Context, string) (messagepkg.HistoryTurn, error) {
	if s.latestErr != nil {
		return messagepkg.HistoryTurn{}, s.latestErr
	}
	return s.latest, nil
}

func (s *replacementOperationMessageService) GetVisibleTurnByMessage(_ context.Context, _ string, messageID string) (messagepkg.HistoryTurn, error) {
	s.turnByMsgID = messageID
	return s.turnByMsg, nil
}

func TestPreflightHookFailureIsRetriedThroughTheHook(t *testing.T) {
	if !isPreflightUserMessageHookFailure(messagepkg.Message{
		Metadata: map[string]any{messagepkg.HistoryFailureOriginMetadataKey: messagepkg.HistoryFailureOriginUserMessageHook},
	}) {
		t.Fatal("Hook failure marker was not recognized")
	}
	if isPreflightUserMessageHookFailure(messagepkg.Message{
		Metadata: map[string]any{messagepkg.HistoryErrorCodeMetadataKey: string(apperror.CodeAgentProviderRequestRejected)},
	}) {
		t.Fatal("provider failure was incorrectly classified as a Hook failure")
	}
}

// The deprecated message-id spelling is resolved to a round at the boundary, so
// a client shipped before the turn-id contract keeps working after an upgrade.
func TestResolveTurnIDForMessageMapsTheLegacySpelling(t *testing.T) {
	messages := &replacementOperationMessageService{
		turnByMsg: messagepkg.HistoryTurn{ID: " turn-old "},
	}
	service := New(Deps{Messages: messages})

	got, err := service.ResolveTurnID(context.Background(), "session-1", " assistant-result ")
	if err != nil {
		t.Fatalf("resolve turn id: %v", err)
	}
	if got != "turn-old" {
		t.Fatalf("turn id = %q, want turn-old", got)
	}
	if messages.turnByMsgID != "assistant-result" {
		t.Fatalf("looked up message %q, want assistant-result", messages.turnByMsgID)
	}

	if _, err := service.ResolveTurnID(context.Background(), "session-1", "  "); err == nil {
		t.Fatal("resolve without a message id: want error")
	}
}

func TestResolveTurnIDForMessageRejectsAMessageWithoutAVisibleTurn(t *testing.T) {
	service := New(Deps{Messages: &replacementOperationMessageService{}})

	if _, err := service.ResolveTurnID(context.Background(), "session-1", "assistant-result"); err == nil {
		t.Fatal("resolve a message with no visible turn: want error")
	}
}

func TestPrepareReplacementOperationUsesPersistedTurnBoundary(t *testing.T) {
	t.Run("retry replaces the original request and output", func(t *testing.T) {
		messages := &replacementOperationMessageService{
			latest: messagepkg.HistoryTurn{
				ID:                 "turn-old",
				RequestMessageID:   "user-request",
				AssistantMessageID: "assistant-first",
			},
		}
		service := New(Deps{Messages: messages})

		got, err := service.PrepareRetry(context.Background(), "session-1", "turn-old")
		if err != nil {
			t.Fatalf("prepare retry operation: %v", err)
		}
		if got != "user-request" {
			t.Fatalf("replace from message = %q, want user-request", got)
		}
	})

	t.Run("edit begins at persisted request message", func(t *testing.T) {
		messages := &replacementOperationMessageService{
			latest: messagepkg.HistoryTurn{
				ID:               "turn-old",
				RequestMessageID: "user-request",
			},
		}
		service := New(Deps{Messages: messages})

		got, err := service.PrepareEdit(context.Background(), "session-1", "turn-old")
		if err != nil {
			t.Fatalf("prepare edit operation: %v", err)
		}
		if got != "user-request" {
			t.Fatalf("replace from message = %q, want user-request", got)
		}
	})

	// The turn id is the whole validation: a client that names an older round
	// is refused without ever naming a stored message.
	t.Run("a turn that is no longer the latest is refused", func(t *testing.T) {
		messages := &replacementOperationMessageService{
			latest: messagepkg.HistoryTurn{
				ID:                 "turn-new",
				RequestMessageID:   "user-request",
				AssistantMessageID: "assistant-first",
			},
		}
		service := New(Deps{Messages: messages})

		_, err := service.PrepareRetry(context.Background(), "session-1", "turn-old")
		if got := apperror.CodeOf(err); got != apperror.CodeSessionTurnNotLatest {
			t.Fatalf("retry on a superseded turn: error code = %q, want %q", got, apperror.CodeSessionTurnNotLatest)
		}
		_, err = service.PrepareEdit(context.Background(), "session-1", "turn-old")
		if got := apperror.CodeOf(err); got != apperror.CodeSessionTurnNotLatest {
			t.Fatalf("edit on a superseded turn: error code = %q, want %q", got, apperror.CodeSessionTurnNotLatest)
		}
	})

	t.Run("an unnamed turn is refused before any lookup", func(t *testing.T) {
		service := New(Deps{Messages: &replacementOperationMessageService{}})

		if _, err := service.PrepareRetry(context.Background(), "session-1", "  "); err == nil {
			t.Fatal("prepare retry operation without a turn id: want error")
		}
	})

	t.Run("retry rejects a turn without its canonical assistant anchor", func(t *testing.T) {
		messages := &replacementOperationMessageService{
			latest: messagepkg.HistoryTurn{
				ID:               "turn-old",
				RequestMessageID: "user-request",
			},
		}
		service := New(Deps{Messages: messages})

		_, err := service.PrepareRetry(context.Background(), "session-1", "turn-old")
		if got := apperror.CodeOf(err); got != apperror.CodeSessionTurnIncomplete {
			t.Fatalf("error code = %q, want %q", got, apperror.CodeSessionTurnIncomplete)
		}
		if apperror.CauseOf(err) == nil {
			t.Fatal("incomplete turn error must carry a private cause for logs")
		}
	})

	// The store's own no-rows wording used to reach the composer verbatim.
	t.Run("a session with no visible turn reports the stale-turn code", func(t *testing.T) {
		messages := &replacementOperationMessageService{latestErr: messagepkg.ErrNoVisibleTurn}
		service := New(Deps{Messages: messages})

		_, err := service.PrepareRetry(context.Background(), "session-1", "turn-old")
		if got := apperror.CodeOf(err); got != apperror.CodeSessionTurnNotLatest {
			t.Fatalf("error code = %q, want %q", got, apperror.CodeSessionTurnNotLatest)
		}
	})
}
