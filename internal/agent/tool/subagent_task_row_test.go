package tools

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type refusingTaskRowService struct {
	*fakeAgentMessageService
	err error
}

func (s refusingTaskRowService) Persist(ctx context.Context, input messagepkg.PersistInput) (messagepkg.Message, error) {
	if input.Role == "user" {
		return messagepkg.Message{}, s.err
	}
	return s.fakeAgentMessageService.Persist(ctx, input)
}

// Every row of a subagent run is filed under its task row. A task row that
// cannot be written is an error for the caller, not an empty id to carry on
// with; a run with no message store has nothing to file and gets no id.
func TestPersistUserMessageReportsAFailedTaskRow(t *testing.T) {
	refused := errors.New("history write failed")
	provider := &SpawnProvider{
		messageService: refusingTaskRowService{fakeAgentMessageService: newFakeAgentMessageService(), err: refused},
		logger:         slog.New(slog.DiscardHandler),
	}
	req := &agentRequest{agentSessionID: "subagent-session", parentSession: SessionContext{BotID: "bot-1"}, message: "task"}
	if id, err := provider.persistUserMessage(t.Context(), req); !errors.Is(err, refused) || id != "" {
		t.Fatalf("persistUserMessage() = (%q, %v), want the store's error", id, err)
	}

	unstored := &SpawnProvider{logger: slog.New(slog.DiscardHandler)}
	if id, err := unstored.persistUserMessage(t.Context(), req); err != nil || id != "" {
		t.Fatalf("persistUserMessage() without a message store = (%q, %v), want no id and no error", id, err)
	}
}
