package turnwire_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
)

func queuedCommand(text string) turn.StartTurnCommand {
	return turn.StartTurnCommand{
		SchemaVersion: turnwire.StartTurnVersion, TeamID: "team-1", Mode: turn.ModeChat,
		BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1",
		Query: text, UserVisibleText: text,
		ReplyTarget: "chat-1", ExternalMessageID: "ext-" + text, UserID: "user-" + text,
		Token: "secret-token", ChatToken: "secret-chat-token",
	}
}

func storedInput(t *testing.T, text string, at time.Time) turnwire.QueuedInput {
	t.Helper()
	claims := auth.RouteCredentials{BotID: "bot-1", ChatID: "bot-1", ReplyTarget: "chat-1", UserID: "user-" + text}
	payload, err := turnwire.NewQueuedInput(queuedCommand(text), &claims, at).Marshal()
	require.NoError(t, err)
	return turnwire.ParseQueuedInput(payload)
}

// What is stored never contains a token: it would expire while the input waits,
// and whoever starts the turn signs a new one from the claims.
func TestStoredInputCarriesNoCredentialsOnlyClaims(t *testing.T) {
	cmd := queuedCommand("hello")
	claims := auth.RouteCredentials{BotID: "bot-1", ChatID: "bot-1", ReplyTarget: "chat-1", UserID: "user-1"}
	payload, err := turnwire.NewQueuedInput(cmd, &claims, time.Unix(100, 0).UTC()).Marshal()
	require.NoError(t, err)
	require.NotContains(t, string(payload), "secret-token")
	require.NotContains(t, string(payload), "secret-chat-token")

	input := turnwire.ParseQueuedInput(payload)
	restored, err := input.StartCommand()
	require.NoError(t, err)
	require.Empty(t, restored.Token)
	require.Empty(t, restored.ChatToken)
	require.Equal(t, "hello", restored.Query)
	require.Equal(t, "ext-hello", restored.ExternalMessageID)
	require.Equal(t, claims, *input.Claims)
	require.True(t, input.ReceivedAt.Equal(time.Unix(100, 0)))
	require.Equal(t, "hello", input.VisibleText())
}

func TestInputWithoutACommandIsListableButNotStartable(t *testing.T) {
	input := turnwire.ParseQueuedInput([]byte(`{"text":"  typed earlier  "}`))
	require.Equal(t, "typed earlier", input.VisibleText())
	_, err := input.StartCommand()
	require.ErrorIs(t, err, turnwire.ErrQueuedInputWithoutCommand)

	unreadable := turnwire.ParseQueuedInput([]byte(`not json`))
	require.Empty(t, unreadable.VisibleText())
	_, err = unreadable.StartCommand()
	require.ErrorIs(t, err, turnwire.ErrQueuedInputWithoutCommand)
}

// A batch is answered by the last input's turn. The earlier inputs stay
// messages of their own, in arrival order, with what made each of them what it
// is; the reply window is the last input's.
func TestMergedBatchIsTheLastTurnWithTheEarlierMessagesBeforeIt(t *testing.T) {
	first := time.Unix(100, 0).UTC()
	last := time.Unix(300, 0).UTC()
	inputs := []turnwire.QueuedInput{
		storedInput(t, "one", first),
		storedInput(t, "two", time.Unix(200, 0).UTC()),
		storedInput(t, "three", last),
	}
	for i, id := range []string{"item-1", "item-2", "item-3"} {
		inputs[i].ItemID = id
	}
	inputs[0].Command.Attachments = []turn.Attachment{{Type: "image", Name: "a.png"}}
	inputs[0].Command.MentionsBot = true
	inputs[1].Command.SourceReplyToMessageID = "quoted-1"
	inputs[2].Command.Attachments = []turn.Attachment{{Type: "file", Name: "c.txt"}}

	merged, err := turnwire.MergeQueued(inputs)
	require.NoError(t, err)
	require.Equal(t, "three", merged.Command.Query, "the run's own message is the last one's alone")
	require.Equal(t, "three", merged.Command.UserVisibleText)
	require.Empty(t, merged.Command.ModelQuery)
	require.Equal(t, "ext-three", merged.Command.ExternalMessageID, "the reply quotes the last message")
	require.Equal(t, "user-three", merged.Claims.UserID)
	require.True(t, merged.ReceivedAt.Equal(last))
	require.Len(t, merged.Command.Attachments, 1)
	require.Equal(t, "c.txt", merged.Command.Attachments[0].Name)

	require.Len(t, merged.Command.Preceding, 2)
	one, two := merged.Command.Preceding[0], merged.Command.Preceding[1]
	require.Equal(t, "item-1", one.ID)
	require.Equal(t, "one", one.Query)
	require.Equal(t, "ext-one", one.ExternalMessageID)
	require.Equal(t, "user-one", one.UserID)
	require.True(t, one.MentionsBot)
	require.Equal(t, "a.png", one.Attachments[0].Name, "an attachment stays with its message")
	require.Equal(t, first.UnixMilli(), one.ReceivedAtMs)
	require.Equal(t, "item-2", two.ID)
	require.Equal(t, "quoted-1", two.SourceReplyToMessageID, "a reply stays with its message")
	require.False(t, two.MentionsBot)
}

func TestQueuedCommandsWithPrecedingMessagesCrossTheWire(t *testing.T) {
	cmd := queuedCommand("last")
	cmd.Preceding = []turn.PrecedingMessage{{ID: "item-1", Query: "earlier", ExternalMessageID: "ext-1", MentionsBot: true}}
	data, err := json.Marshal(turnwire.FromStartTurn(cmd))
	require.NoError(t, err)
	var wire turnwire.StartTurn
	require.NoError(t, json.Unmarshal(data, &wire))
	got, err := wire.Command()
	require.NoError(t, err)
	require.Equal(t, cmd.Preceding, got.Preceding)
}

func TestBatchOfOneIsTheInputUnchanged(t *testing.T) {
	only := storedInput(t, "alone", time.Unix(5, 0).UTC())
	merged, err := turnwire.MergeQueued([]turnwire.QueuedInput{only})
	require.NoError(t, err)
	want, err := only.StartCommand()
	require.NoError(t, err)
	require.Equal(t, want, merged.Command)
}

func TestMergeRefusesEmptyBatchAndInputWithoutACommand(t *testing.T) {
	_, err := turnwire.MergeQueued(nil)
	require.Error(t, err)
	_, err = turnwire.MergeQueued([]turnwire.QueuedInput{storedInput(t, "ok", time.Now()), {Text: "typed earlier"}})
	require.ErrorIs(t, err, turnwire.ErrQueuedInputWithoutCommand)
}
