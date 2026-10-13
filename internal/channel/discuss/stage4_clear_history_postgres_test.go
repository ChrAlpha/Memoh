package discuss

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

var (
	stage4MigrationOnce sync.Once
	stage4MigrationErr  error
)

func openStage4Postgres(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the discuss PostgreSQL integration")
	}
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if os.Getenv("TEST_POSTGRES_BOOTSTRAP_SCHEMA") == "1" {
		stage4MigrationOnce.Do(func() { stage4MigrationErr = dbtest.MigratePostgresUp(dsn) })
		if stage4MigrationErr != nil {
			t.Fatalf("migrate PostgreSQL test database: %v", stage4MigrationErr)
		}
	}
	return pool
}

func createStage4Session(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (botID, sessionID string) {
	t.Helper()
	userID, bot, session := uuid.New(), uuid.New(), uuid.New()
	name := "stage4-discuss-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, bot, userID, name); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')`, session, bot); err != nil {
		t.Fatalf("create session: %v", err)
	}
	cleanup := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", bot)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})
	return bot.String(), session.String()
}

// recordingEventLoader passes reads through to the real event store and keeps
// what each read returned, which is what the worker renders from.
type recordingEventLoader struct {
	inner *timeline.EventStore
	mu    sync.Mutex
	reads [][]timeline.CanonicalEvent
}

func (r *recordingEventLoader) LoadEventsForReplay(ctx context.Context, botID, sessionID string) ([]timeline.CanonicalEvent, error) {
	events, err := r.inner.LoadEventsForReplay(ctx, botID, sessionID)
	r.mu.Lock()
	r.reads = append(r.reads, events)
	r.mu.Unlock()
	return events, err
}

func (r *recordingEventLoader) snapshot() [][]timeline.CanonicalEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]timeline.CanonicalEvent(nil), r.reads...)
}

// Clearing a session's history deletes its events in the same statement
// (ruling 13). The discuss worker rebuilds its render context from the event
// table on every signal, so an event left behind would reappear as fresh
// input after the clear. The path runs against real PostgreSQL: the event
// store, the history clear, and the driver's worker.
func TestDiscussDoesNotResurrectMessagesBeforeClearHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	pool := openStage4Postgres(t, ctx)
	botID, sessionID := createStage4Session(t, ctx, pool)
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	store := timeline.NewEventStore(nil, queries)
	messages := messagepkg.NewService(nil, queries)

	persist := func(messageID, text string, at int64) {
		t.Helper()
		_, _, err := store.PersistEvent(ctx, botID, sessionID, timeline.MessageEvent{
			SessionID: sessionID, MessageID: messageID, ReceivedAtMs: at, TimestampSec: at / 1000,
			Content: []timeline.ContentNode{{Type: "text", Text: text}},
		})
		if err != nil {
			t.Fatalf("persist event %s: %v", messageID, err)
		}
	}

	loader := &recordingEventLoader{inner: store}
	driver := NewDiscussDriver(DiscussDriverDeps{Events: loader})
	defer driver.StopAll()
	cfg := DiscussSessionConfig{BotID: botID, ThreadID: sessionID}
	now := time.Now().UnixMilli()

	persist("before-1", "BEFORE-CLEAR-MARKER", now)
	driver.Notify(sessionID, cfg)
	waitFor(t, func() bool { return len(loader.snapshot()) >= 1 })
	rc, err := driver.loadRC(ctx, botID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(timeline.RCToXML(rc), "BEFORE-CLEAR-MARKER") {
		t.Fatal("the message sent before the clear is not in the context to begin with")
	}

	if err := messages.DeleteBySession(ctx, sessionID); err != nil {
		t.Fatalf("clear history: %v", err)
	}
	persist("after-1", "AFTER-CLEAR-MARKER", now+1000)
	readsBefore := len(loader.snapshot())
	driver.Notify(sessionID, cfg)
	waitFor(t, func() bool { return len(loader.snapshot()) > readsBefore })

	reads := loader.snapshot()
	last := reads[len(reads)-1]
	rendered := timeline.RCToXML(timeline.BuildRC(last, timeline.RenderParams{}))
	if strings.Contains(rendered, "BEFORE-CLEAR-MARKER") {
		t.Fatalf("the worker's read after the clear still holds the cleared message:\n%s", rendered)
	}
	if !strings.Contains(rendered, "AFTER-CLEAR-MARKER") {
		t.Fatalf("the worker's read after the clear lacks the new message:\n%s", rendered)
	}
}
