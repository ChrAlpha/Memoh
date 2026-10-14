//go:build integration

package inbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// failingPassiveRows fails the row write after the event insert has run in the
// same transaction, so the test can observe whether the event rolled back.
type failingPassiveRows struct{}

func (failingPassiveRows) PersistWithin(context.Context, dbstore.Queries, message.PersistInput) (message.Message, error) {
	return message.Message{}, errors.New("row write unavailable")
}

func (failingPassiveRows) PublishCreated(message.Message) {}

func TestPostgresPassiveRecordCommitsEventAndRowTogether(t *testing.T) {
	ctx := context.Background()
	pool := openPassiveRowPostgresPool(t, ctx)
	botID, sessionID := createPassiveRowFixtures(t, ctx, pool)
	processor, _ := newPassiveRecordProcessor(t, pool)

	committed, inserted, err := processor.recordPassiveInbound(ctx, passiveRecordIdentity(botID), passiveRecordMessage("m-commit"), "hello", nil, "", sessionID, passiveRecordEvent(sessionID, "m-commit"))
	if err != nil || !committed || !inserted {
		t.Fatalf("record: committed=%v inserted=%v err=%v", committed, inserted, err)
	}
	if got := countPassiveRows(t, ctx, pool, "bot_session_events", sessionID); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	if got := countPassiveRows(t, ctx, pool, "bot_history_messages", sessionID); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	var linked int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND event_id IS NOT NULL", sessionID).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Fatalf("rows linked to an event = %d, want 1", linked)
	}
}

func TestPostgresPassiveRecordRollsBackEventWhenRowFails(t *testing.T) {
	ctx := context.Background()
	pool := openPassiveRowPostgresPool(t, ctx)
	botID, sessionID := createPassiveRowFixtures(t, ctx, pool)
	processor, _ := newPassiveRecordProcessor(t, pool)
	processor.passiveRows = failingPassiveRows{}

	_, _, err := processor.recordPassiveInbound(ctx, passiveRecordIdentity(botID), passiveRecordMessage("m-rollback"), "hello", nil, "", sessionID, passiveRecordEvent(sessionID, "m-rollback"))
	if err == nil {
		t.Fatal("expected the row failure to surface")
	}
	if got := countPassiveRows(t, ctx, pool, "bot_session_events", sessionID); got != 0 {
		t.Fatalf("event survived a rolled-back transaction: %d", got)
	}
}

func TestPostgresPassiveRecordReplayWritesOneEventAndOneRow(t *testing.T) {
	ctx := context.Background()
	pool := openPassiveRowPostgresPool(t, ctx)
	botID, sessionID := createPassiveRowFixtures(t, ctx, pool)
	processor, _ := newPassiveRecordProcessor(t, pool)

	for i := 0; i < 2; i++ {
		committed, inserted, err := processor.recordPassiveInbound(ctx, passiveRecordIdentity(botID), passiveRecordMessage("m-replay"), "hello", nil, "", sessionID, passiveRecordEvent(sessionID, "m-replay"))
		if err != nil || !committed {
			t.Fatalf("attempt %d: committed=%v err=%v", i, committed, err)
		}
		if want := i == 0; inserted != want {
			t.Fatalf("attempt %d: inserted=%v, want %v", i, inserted, want)
		}
	}
	if got := countPassiveRows(t, ctx, pool, "bot_session_events", sessionID); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	if got := countPassiveRows(t, ctx, pool, "bot_history_messages", sessionID); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}

func TestPostgresPassiveRecordConcurrentSameMessageWritesOnce(t *testing.T) {
	ctx := context.Background()
	pool := openPassiveRowPostgresPool(t, ctx)
	botID, sessionID := createPassiveRowFixtures(t, ctx, pool)
	processor, _ := newPassiveRecordProcessor(t, pool)

	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := processor.recordPassiveInbound(ctx, passiveRecordIdentity(botID), passiveRecordMessage("m-race"), "hello", nil, "", sessionID, passiveRecordEvent(sessionID, "m-race"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent record: %v", err)
		}
	}
	if got := countPassiveRows(t, ctx, pool, "bot_session_events", sessionID); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	if got := countPassiveRows(t, ctx, pool, "bot_history_messages", sessionID); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}

func newPassiveRecordProcessor(t *testing.T, pool *pgxpool.Pool) (*ChannelInboundProcessor, *message.DBService) {
	t.Helper()
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	log := slog.Default()
	events := timeline.NewEventStore(log, queries)
	msgSvc := message.NewService(nil, queries)
	processor := NewChannelInboundProcessor(log, nil, nil, nil, nil, nil, nil, "", 0)
	processor.SetPipeline(events, msgSvc)
	return processor, msgSvc
}

func passiveRecordIdentity(botID string) InboundIdentity {
	return InboundIdentity{BotID: botID}
}

func passiveRecordMessage(externalID string) channel.InboundMessage {
	return channel.InboundMessage{
		BotID:        "",
		Channel:      channel.ChannelType("telegram"),
		Message:      channel.Message{ID: externalID, Text: "hello"},
		ReplyTarget:  "group-target",
		Conversation: channel.Conversation{ID: "g", Type: channel.ConversationTypeGroup},
	}
}

func passiveRecordEvent(sessionID, externalID string) timeline.CanonicalEvent {
	event := inboundTestEvent()
	event.SessionID = sessionID
	event.MessageID = externalID
	return event
}

func countPassiveRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, sessionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE session_id = $1", table), sessionID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func openPassiveRowPostgresPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("MEMOH_TEST_POSTGRES_REQUIRED") == "1" {
			t.Fatal("passive row postgres tests are required, but TEST_POSTGRES_DSN is not set")
		}
		t.Skip("skip passive row postgres test: TEST_POSTGRES_DSN is not set")
	}
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// createPassiveRowFixtures returns the bot and session ids as strings. The
// session is a discuss-free chat session, so the recording path needs no
// session runtime.
func createPassiveRowFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	userID := uuid.New()
	botID := uuid.New()
	sessionID := uuid.New()
	name := fmt.Sprintf("passive-row-test-%s", uuid.NewString())
	if _, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active)
			VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role)
		SELECT id, 'admin' FROM created_user`, userID, name); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)", botID, userID, name); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO bot_sessions (id, bot_id, channel_type) VALUES ($1, $2, 'local')", sessionID, botID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM bots WHERE id = $1", botID)
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID)
	})
	return botID.String(), sessionID.String()
}
