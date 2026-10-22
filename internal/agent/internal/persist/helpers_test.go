package persist

import (
	"context"
	"errors"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// newTestWriter is a Writer over d, with a discarding logger and a bot
// environment that has no settings unless d names one.
func newTestWriter(d Deps) *Writer {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Messages == nil {
		d.Messages = &agenttest.RecordingMessages{}
	}
	if d.Sessions == nil {
		d.Sessions = &agenttest.Sessions{GetFn: func(context.Context, string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{}, errors.New("session not found")
		}}
	}
	if d.Runtime == nil {
		d.Runtime = unusedRuntime{}
	}
	if d.Bot == nil {
		d.Bot = agenttest.BotEnv(d.Logger)
	}
	return New(d)
}

// unusedRuntime stands in for a session runtime a test never reaches; any call
// into it panics.
type unusedRuntime struct{ SteerRuntime }

const storeRoundBotID = "11111111-1111-1111-1111-111111111111"

func int64Pointer(value int64) *int64 { return &value }

// recordedMessages is what a message service that stores inputs verbatim
// returns for them.
func recordedMessages(inputs []messagepkg.PersistInput) []messagepkg.Message {
	messages := make([]messagepkg.Message, 0, len(inputs))
	for _, input := range inputs {
		messages = append(messages, messagepkg.Message{
			BotID:          input.BotID,
			SessionID:      input.SessionID,
			Role:           input.Role,
			Content:        input.Content,
			DisplayContent: input.DisplayText,
			Metadata:       input.Metadata,
			TurnID:         runreq.FirstNonEmpty(input.TurnID, "test-turn"),
		})
	}
	return messages
}
