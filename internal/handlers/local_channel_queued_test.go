package handlers

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/web"
	"github.com/felinics/memoh/internal/auth"
	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// A Web send that meets a busy session is answered with run_queued, not with a
// refusal: the frame names the send by the invocation the client sent, carries
// the send's place in the queue, and no run is announced for it. The tests run
// against PostgreSQL because the queue is the table; each is skipped unless
// TEST_POSTGRES_DSN names a migrated database.
func TestAdmitWSRunAnswersASendQueuedBehindARunWithRunQueued(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the queued Web send test")
	}
	ctx := context.Background()
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	userID, botID, sessionID := uuid.New(), uuid.New(), uuid.New()
	name := "ws-queued-" + uuid.NewString()
	_, err = pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, botID, userID, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'web', 'model')`, sessionID, botID)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", botID)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})

	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: "owner-ws-queued", OwnerLeaseTTL: time.Minute, Ledger: sessionledger.New(), Fence: wsAdmissionFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	service := newTestAgentService(t, agent.Deps{
		SessionRuntime: manager,
		QueuedInputs:   sessionruntime.NewQueuedInputs(dbsqlc.New(pool), pool),
	})
	handler := &LocalChannelHandler{logger: slog.New(slog.DiscardHandler), webRuns: service, queue: service, runtimeOps: service, decisions: service, sessionRuntime: manager}
	writer := &wsWriter{ch: make(chan []byte, 32), stop: make(chan struct{}), done: make(chan struct{})}
	h := wsRunHarness{handler: handler, manager: manager, writer: writer}

	queueTurn := func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error) {
		return turn.StartTurnCommand{
				SchemaVersion: 1, TeamID: "team", Mode: turn.ModeChat, BotID: botID.String(), ChatID: botID.String(),
				ThreadID: sessionID.String(), Query: "typed while it runs", UserVisibleText: "typed while it runs",
				CurrentChannel: "web", ReplyTarget: botID.String(), ConversationType: "private",
			},
			&auth.RouteCredentials{BotID: botID.String(), ChatID: botID.String(), ReplyTarget: botID.String()}, nil
	}
	admit := func(ref wsTurnRef, queue func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error)) (*web.Admission, wsTurnRef) {
		return handler.admitWSRun(ctx, writer, botID.String(), "", ref, "ws.message", []byte(`{"kind":"message","text":"x"}`), emptyWSAdmissionView, queue, nil)
	}

	// A run holds the session.
	first := wsTurn("invocation-1", sessionID.String())
	running, _ := admit(first, queueTurn)
	require.NotNil(t, running, "the first send takes the idle session")
	require.NotEmpty(t, h.frames(t))

	queuedRef := wsTurn("invocation-2", sessionID.String())
	admitted, _ := admit(queuedRef, queueTurn)
	require.Nil(t, admitted, "a queued send has no run to execute")
	frames := h.frames(t)
	require.Len(t, frames, 1)
	require.Equal(t, "run_queued", frames[0]["type"])
	require.Equal(t, "invocation-2", frames[0]["invocation_id"])
	require.Equal(t, sessionID.String(), frames[0]["session_id"])
	require.NotEmpty(t, frames[0]["queue_item_id"])
	require.NotContains(t, frames[0], "run_id", "no run is announced for a send that waits")
	require.NotContains(t, frames[0], "duplicate")

	again, _ := admit(queuedRef, queueTurn)
	require.Nil(t, again)
	frames = h.frames(t)
	require.Len(t, frames, 1)
	require.Equal(t, "run_queued", frames[0]["type"])
	require.Equal(t, true, frames[0]["duplicate"], "a repeated send is answered as the repeat it is")

	// A send the queue cannot replay is refused as busy, as before.
	refused, _ := admit(wsTurn("invocation-3", sessionID.String()), nil)
	require.Nil(t, refused)
	frames = h.frames(t)
	require.Len(t, frames, 1)
	require.Equal(t, "run_rejected", frames[0]["type"])
	require.Equal(t, "session_runtime.session_busy", frames[0]["code"])
}
