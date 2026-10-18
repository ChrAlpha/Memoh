package pending

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/admit"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// noDiscuss is the store of a bot with no discuss session: admission has
// nothing to answer once the queue is drained.
type noDiscuss struct{ Queries }

func (noDiscuss) GetDiscussSession(context.Context, dbsqlc.GetDiscussSessionParams) (dbsqlc.GetDiscussSessionRow, error) {
	return dbsqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}

// oneBatch hands out one batch and records what became of it.
type oneBatch struct {
	Store
	batch   sessionruntime.QueueBatch
	claimed bool
	settled int
}

func (o *oneBatch) ClaimBatch(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin) (sessionruntime.QueueBatch, bool, error) {
	if o.claimed {
		return sessionruntime.QueueBatch{}, false, nil
	}
	o.claimed = true
	return o.batch, true, nil
}

func (o *oneBatch) SettleBatch(context.Context, sessionruntime.QueueBatch) error {
	o.settled++
	return nil
}

// recordedTurns remembers the command a batch was started as.
type recordedTurns struct{ started []turn.StartTurnCommand }

func (r *recordedTurns) StartTurn(_ context.Context, cmd turn.StartTurnCommand) (turn.RunHandle, error) {
	r.started = append(r.started, cmd)
	return nil, nil
}

// startedAs queues one input stored with claims and returns the command its
// batch was started as.
func startedAs(t *testing.T, claims *auth.RouteCredentials) turn.StartTurnCommand {
	t.Helper()
	cmd := turn.StartTurnCommand{
		SchemaVersion: turnwire.StartTurnVersion, TeamID: "team-1", Mode: turn.ModeChat,
		BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", IdempotencyKey: "m1",
		Query: "hello", UserVisibleText: "hello", ReplyTarget: "im-chat-1",
	}
	payload, err := turnwire.NewQueuedInput(cmd, claims, time.Now()).Marshal()
	require.NoError(t, err)
	require.Empty(t, turnwire.ParseQueuedInput(payload).Command.Token, "the queue never stores the turn's credentials")
	require.NotContains(t, string(payload), "queue-test-secret")

	key := sessionruntime.Key{BotID: "bot-1", SessionID: "session-1"}
	queue := &oneBatch{batch: sessionruntime.QueueBatch{ID: "batch-1", Key: key, Items: []sessionruntime.QueuedInput{{
		ID: "item-1", BotID: "bot-1", SessionID: "session-1", InvocationID: "m1", Payload: payload,
	}}}}
	turns := &recordedTurns{}
	inputs := New(Deps{
		Logger: slog.New(slog.DiscardHandler), Queries: noDiscuss{}, Queue: queue, Turns: turns,
		Signer: admit.NewSigner("queue-test-secret"),
	})
	admission, err := inputs.CheckAndAdmitDiscuss(t.Context(), "bot-1", "session-1")
	require.NoError(t, err)
	require.Equal(t, DiscussBusy, admission, "a started batch holds the session")
	require.Equal(t, 1, queue.settled)
	require.Len(t, turns.started, 1)
	return turns.started[0]
}

// A turn that was queued is started with credentials signed when it starts,
// from the identities stored with it.
func TestQueuedTurnIsSignedWhenItStartsNotWhenItWasQueued(t *testing.T) {
	started := startedAs(t, &auth.RouteCredentials{BotID: "bot-1", ChatID: "bot-1", ReplyTarget: "im-chat-1", UserID: "user-1"})
	require.NotEmpty(t, started.ChatToken, "a turn that answers somewhere gets a chat token")
	require.NotEmpty(t, started.Token)
}

// A text typed into the queue panel never had credentials.
func TestQueuedTurnWithoutIdentitiesStartsUnsigned(t *testing.T) {
	started := startedAs(t, nil)
	require.Empty(t, started.ChatToken)
	require.Empty(t, started.Token)
}

// A sender no account is bound to starts without, as an inbound turn does.
func TestQueuedTurnOfAnUnboundSenderStartsUnsigned(t *testing.T) {
	started := startedAs(t, &auth.RouteCredentials{BotID: "bot-1", ChatID: "bot-1", ReplyTarget: "im-chat-1"})
	require.Empty(t, started.ChatToken)
}
