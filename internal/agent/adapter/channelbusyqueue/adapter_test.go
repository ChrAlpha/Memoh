package channelbusyqueue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/channel/inbound"
	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// These tests run the adapter over the PostgreSQL queue it adapts. Each is
// skipped unless TEST_POSTGRES_DSN names a migrated database.

type env struct {
	pool      *pgxpool.Pool
	adapter   *Adapter
	botID     string
	sessionID string
}

func newEnv(t *testing.T) env {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the busy queue adapter tests")
	}
	ctx := context.Background()
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	userID, botID, sessionID := uuid.New(), uuid.New(), uuid.New()
	name := "busy-queue-" + uuid.NewString()
	_, err = pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, botID, userID, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')`, sessionID, botID)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", botID)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})
	return env{
		pool:      pool,
		adapter:   New(sessionruntime.NewQueuedInputs(dbsqlc.New(pool), pool)),
		botID:     botID.String(),
		sessionID: sessionID.String(),
	}
}

func (e env) input(origin inbound.BusyOrigin, invocation, mergeKey, text string) inbound.BusyInput {
	return inbound.BusyInput{
		BotID: e.botID, SessionID: e.sessionID, Origin: origin, InvocationID: invocation, MergeKey: mergeKey,
		Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)),
	}
}

func (e env) stateOf(t *testing.T, invocation string) string {
	t.Helper()
	var state string
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT state FROM session_queued_inputs WHERE session_id = $1 AND invocation_id = $2`, e.sessionID, invocation).Scan(&state))
	return state
}

// The adapter hands the channel process only its own platform messages, as a
// batch with what the settle needs, and keeps the server's follow-ups out.
func TestAdapterClaimsOnlyPlatformMessagesAndSettlesThem(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginFollowUp, "panel", "", "typed in the panel")))
	require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "tg-1", "alice", "first")))
	require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "tg-2", "alice", "second")))

	sessions, err := e.adapter.Drainable(ctx)
	require.NoError(t, err)
	require.Contains(t, sessions, inbound.BusySession{BotID: e.botID, SessionID: e.sessionID})

	batch, ok, err := e.adapter.ClaimBatch(ctx, e.botID, e.sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEmpty(t, batch.ID)
	require.NotEmpty(t, batch.ClaimToken)
	require.False(t, batch.Recovered)
	require.Len(t, batch.Inputs, 2, "one sender's neighbours are one batch")
	require.JSONEq(t, `{"text":"first"}`, string(batch.Inputs[0].Payload))
	require.JSONEq(t, `{"text":"second"}`, string(batch.Inputs[1].Payload))
	require.Equal(t, "queued", e.stateOf(t, "panel"), "the follow-up is the server's")

	_, ok, err = e.adapter.ClaimBatch(ctx, e.botID, e.sessionID)
	require.NoError(t, err)
	require.False(t, ok, "a claim of this origin is live")

	require.NoError(t, e.adapter.SettleBatch(ctx, batch))
	require.Equal(t, "consumed", e.stateOf(t, "tg-1"))
	require.Equal(t, "consumed", e.stateOf(t, "tg-2"))
}

func TestAdapterReleaseGivesTheBatchBackAndRejectEndsAnInput(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "tg-1", "", "one")))
	require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "tg-2", "", "two")))

	batch, ok, err := e.adapter.ClaimBatch(ctx, e.botID, e.sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, e.adapter.ReleaseBatch(ctx, batch))
	require.Equal(t, "queued", e.stateOf(t, "tg-1"))

	batch, ok, err = e.adapter.ClaimBatch(ctx, e.botID, e.sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, batch.Inputs, 1, "inputs without a merge key stand alone")
	require.NoError(t, e.adapter.Reject(ctx, batch.Inputs[0].ID))
	require.Equal(t, "cancelled", e.stateOf(t, "tg-1"))
}

func TestAdapterMapsTheQueueErrorsTheChannelTellsApart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "tg-1", "", "one")))
	err := e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "tg-1", "", "another text under the same identity"))
	require.ErrorIs(t, err, inbound.ErrBusyQueueConflict)

	for i := range sessionruntime.MaxPendingQueueItems - 1 {
		require.NoError(t, e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, fmt.Sprintf("fill-%d", i), "", "filler")))
	}
	err = e.adapter.Enqueue(ctx, e.input(inbound.BusyOriginPlatform, "one-too-many", "", "overflow"))
	require.ErrorIs(t, err, inbound.ErrBusyQueueFull)
}
