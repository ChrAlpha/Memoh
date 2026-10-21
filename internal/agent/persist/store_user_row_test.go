package persist

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

var errUserRowRefused = errors.New("user row refused")

// userRowRefusingMessages fails every user row and stores the rest.
type userRowRefusingMessages struct {
	*agenttest.RecordingMessages
}

func (m userRowRefusingMessages) Persist(ctx context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	if input.Role == "user" {
		return messagepkg.Message{}, errUserRowRefused
	}
	return m.RecordingMessages.Persist(ctx, input)
}

// The user row is the request every later row of the round is filed under. When
// it cannot be written, the round fails and none of the later rows is written.
func TestStoreRoundFailsWhenTheUserRowCannotBeWritten(t *testing.T) {
	t.Parallel()
	recorded := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: userRowRefusingMessages{recorded}})
	round := []runreq.ModelMessage{
		{Role: "user", Content: runreq.NewTextContent("hello")},
		{Role: "assistant", Content: runreq.NewTextContent("answer")},
	}

	persisted, err := writer.StoreRoundResult(t.Context(), runreq.ChatRequest{
		BotID: storeRoundBotID, ThreadID: "session-1", RunID: "run-1", Query: "hello",
	}, round, "model-1", RoundOptions{})
	if !errors.Is(err, errUserRowRefused) {
		t.Fatalf("StoreRoundResult() error = %v, want the user row's error", err)
	}
	if len(persisted) != 0 || len(recorded.Persisted) != 0 {
		t.Fatalf("rows written after the user row failed: returned %d, stored %d", len(persisted), len(recorded.Persisted))
	}
}

// A run row that finds no request row ends the round the same way.
func TestStoreRoundFailsWhenARunRowFindsNoRequestRow(t *testing.T) {
	t.Parallel()
	recorded := &agenttest.RecordingMessages{}
	writer := newTestWriter(Deps{Messages: runRowRejectingMessages{recorded}})
	round := []runreq.ModelMessage{
		{Role: "assistant", Content: runreq.NewTextContent("first")},
		{Role: "assistant", Content: runreq.NewTextContent("second")},
	}

	_, err := writer.StoreRoundResult(t.Context(), runreq.ChatRequest{
		BotID: storeRoundBotID, ThreadID: "session-1", RunID: "run-1", Query: "hello", UserMessagePersisted: true,
	}, round, "model-1", RoundOptions{})
	if !errors.Is(err, messagepkg.ErrRunRowTurnNotFound) {
		t.Fatalf("StoreRoundResult() error = %v, want ErrRunRowTurnNotFound", err)
	}
	if len(recorded.Persisted) != 0 {
		t.Fatalf("rows written after a row found no request: %d", len(recorded.Persisted))
	}
}

// runRowRejectingMessages is the store's answer to a run row with no request.
type runRowRejectingMessages struct {
	*agenttest.RecordingMessages
}

func (m runRowRejectingMessages) Persist(ctx context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	if input.RunID != "" && input.TurnRequestMessageID == "" && input.Role != "user" {
		return messagepkg.Message{}, messagepkg.ErrRunRowTurnNotFound
	}
	return m.RecordingMessages.Persist(ctx, input)
}
