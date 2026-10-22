package pending

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/agentapi"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

func queueInputOf(text string) agentapi.QueueInput {
	return agentapi.QueueInput{
		TeamID: "team-1", BotID: "bot-1", SessionID: "session-1", InvocationID: "invoke-1",
		UserID: "user-1", SourceChannelIdentityID: "identity-1", Text: text,
	}
}

// A queued text outlives the request that carried it, so an input without the
// identity the continuation needs is refused at the door, for follow-ups and
// steers alike.
func TestQueueInputRequiresRecordedTeam(t *testing.T) {
	inputs := New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Queue: &gated{}, Continuations: noContinuations{}})
	ctx := t.Context()

	for name, edit := range map[string]func(*agentapi.QueueInput){
		"team":    func(in *agentapi.QueueInput) { in.TeamID = "" },
		"bot":     func(in *agentapi.QueueInput) { in.BotID = " " },
		"session": func(in *agentapi.QueueInput) { in.SessionID = "" },
	} {
		input := queueInputOf("hello")
		edit(&input)
		_, err := inputs.EnqueueFollowUp(ctx, input)
		require.ErrorIs(t, err, agentapi.ErrQueueInputIncomplete, "follow-up without %s", name)
		_, err = inputs.EnqueueSteer(ctx, input)
		require.ErrorIs(t, err, agentapi.ErrQueueInputIncomplete, "steer without %s", name)
	}

	blank := queueInputOf("  ")
	_, err := inputs.EnqueueFollowUp(ctx, blank)
	require.ErrorIs(t, err, sessionruntime.ErrQueueInvalidReference)
	noInvocation := queueInputOf("hello")
	noInvocation.InvocationID = ""
	_, err = inputs.EnqueueFollowUp(ctx, noInvocation)
	require.ErrorIs(t, err, sessionruntime.ErrQueueInvalidReference)
}

// The command stored at enqueue is what the batch is started as: its team,
// sender and routing come from the record, not from the process that starts it.
func TestQueuedTextFollowUpReplaysRecordedIdentity(t *testing.T) {
	input := queueInputOf("hello")
	input.TeamID, input.UserID, input.SourceChannelIdentityID = "team-cloud", "user-9", "identity-9"
	cmd, err := queueTurnCommand(input)
	require.NoError(t, err)
	payload, err := turnwire.NewQueuedInput(cmd, nil, time.Time{}).Marshal()
	require.NoError(t, err)

	key := sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"}
	queue := &oneBatch{batch: sessionruntime.QueueBatch{ID: "batch-1", Key: key, Items: []sessionruntime.QueuedInput{{
		ID: "item-1", BotID: "bot-1", SessionID: "session-1", InvocationID: "invoke-1", Payload: payload,
	}}}}
	turns := &recordedTurns{}
	inputs := New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Queue: queue, Turns: turns, Continuations: noContinuations{}})

	started, _, err := inputs.drainFollowUps(t.Context(), key)
	require.NoError(t, err)
	require.True(t, started)
	require.Len(t, turns.started, 1)
	got := turns.started[0]
	require.Equal(t, "hello", got.Query)
	require.Equal(t, "team-cloud", got.TeamID)
	require.Equal(t, "user-9", got.UserID)
	require.Equal(t, "identity-9", got.SourceChannelIdentityID)
	require.Equal(t, "bot-1", got.BotID)
	require.Equal(t, "bot-1", got.ChatID)
	require.Equal(t, "session-1", got.ThreadID)
}

// An input that cannot be restored says which part is missing.
func TestRestoreInputReportsWhatAnItemIsMissing(t *testing.T) {
	item := func(payload []byte) sessionruntime.QueuedInput {
		return sessionruntime.QueuedInput{ID: "f1", BotID: "bot", SessionID: "session", Payload: payload}
	}
	encode := func(cmd turn.StartTurnCommand) []byte {
		payload, err := turnwire.NewQueuedInput(cmd, nil, time.Time{}).Marshal()
		require.NoError(t, err)
		return payload
	}
	restored, err := RestoreInput(item(encode(turn.StartTurnCommand{SchemaVersion: turnwire.StartTurnVersion, TeamID: "team1", BotID: "bot", ThreadID: "session", Query: "hello"})))
	require.NoError(t, err)
	require.Equal(t, "team1", restored.Command.TeamID)
	require.Equal(t, "hello", restored.Command.Query)

	for name, tc := range map[string]struct {
		payload []byte
		want    error
	}{
		"command for another session": {encode(turn.StartTurnCommand{SchemaVersion: turnwire.StartTurnVersion, TeamID: "team1", BotID: "bot", ThreadID: "other", Query: "x"}), errQueuedCommandForeignSession},
		"command without team":        {encode(turn.StartTurnCommand{SchemaVersion: turnwire.StartTurnVersion, BotID: "bot", ThreadID: "session", Query: "x"}), errQueuedCommandWithoutTeam},
		"command without input":       {encode(turn.StartTurnCommand{SchemaVersion: turnwire.StartTurnVersion, TeamID: "team1", BotID: "bot", ThreadID: "session"}), errQueuedCommandWithoutInput},
	} {
		_, err := RestoreInput(item(tc.payload))
		require.ErrorIs(t, err, tc.want, name)
	}
	for name, payload := range map[string][]byte{
		"text-only payload from an earlier release": []byte(`{"text":"hello"}`),
		"unparseable payload":                       []byte(`{"text":`),
	} {
		_, err := RestoreInput(item(payload))
		require.Error(t, err, name)
	}
}

// rejectFails is a queue that cannot record a rejection and hands out one batch.
type rejectFails struct {
	oneBatch
	rejects int
}

func (r *rejectFails) Reject(context.Context, string, string) error {
	r.rejects++
	return errors.New("injected reject failure")
}

// When the rejection of an input cannot be recorded, the drain tries it once,
// starts nothing and returns; the claimed item is not handed out again until
// its lease ends.
func TestDrainStopsWhenRejectionCannotBeRecorded(t *testing.T) {
	key := sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"}
	queue := &rejectFails{oneBatch: oneBatch{batch: sessionruntime.QueueBatch{ID: "batch-1", Key: key, Items: []sessionruntime.QueuedInput{{
		ID: "legacy", BotID: "bot-1", SessionID: "session-1", InvocationID: "legacy", Payload: []byte(`{"text":"no command"}`),
	}}}}}
	turns := &recordedTurns{}
	inputs := New(Deps{Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Queue: queue, Turns: turns, Continuations: noContinuations{}})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started, deferred, err := inputs.drainFollowUps(ctx, key)
	require.NoError(t, err)
	require.False(t, started)
	require.False(t, deferred)
	require.Equal(t, 1, queue.rejects)
	require.Empty(t, turns.started)
}
