package tools

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

var (
	historyToolsMigrationOnce sync.Once
	historyToolsMigrationErr  error
)

func openHistoryToolsPostgres(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the history tools PostgreSQL integration")
	}
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if os.Getenv("TEST_POSTGRES_BOOTSTRAP_SCHEMA") == "1" {
		historyToolsMigrationOnce.Do(func() {
			historyToolsMigrationErr = dbtest.MigratePostgresUp(dsn)
		})
		if historyToolsMigrationErr != nil {
			t.Fatalf("migrate PostgreSQL test database: %v", historyToolsMigrationErr)
		}
	}
	return pool
}

// createHistoryToolsFixture inserts a user, a bot and one session with random
// identities, and removes them when the test ends.
func createHistoryToolsFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (botID, sessionID string) {
	t.Helper()
	userID, bot, session := uuid.New(), uuid.New(), uuid.New()
	name := "history-tools-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name); err != nil {
		t.Fatalf("create user fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, bot, userID, name); err != nil {
		t.Fatalf("create bot fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')`, session, bot); err != nil {
		t.Fatalf("create session fixture: %v", err)
	}
	cleanupCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM bots WHERE id = $1", bot)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", userID)
	})
	return bot.String(), session.String()
}

// The model reads message history through get_messages and search_messages, so
// a row refused at admission must not come back through either one: not in the
// latest page, not before a cursor, not by id, and not by a keyword that only
// it contains.
func TestPostgresHistoryToolsLeaveRefusedRowsOut(t *testing.T) {
	ctx := context.Background()
	pool := openHistoryToolsPostgres(t, ctx)
	botID, sessionID := createHistoryToolsFixture(t, ctx, pool)
	queries := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	messages := messagepkg.NewService(nil, queries)
	provider := NewHistoryProvider(nil, nil, messages, queries)
	sess := SessionContext{BotID: botID, SessionID: sessionID}

	persist := func(role, text string, refused bool) string {
		t.Helper()
		content, err := json.Marshal(map[string]string{"role": role, "content": text})
		if err != nil {
			t.Fatal(err)
		}
		var admission messagepkg.Admission
		if refused {
			admission = messagepkg.AdmissionRejectedACL
		}
		row, err := messages.Persist(ctx, messagepkg.PersistInput{
			BotID: botID, SessionID: sessionID, Role: role, Content: content, Admission: admission,
		})
		if err != nil {
			t.Fatalf("persist %q: %v", text, err)
		}
		return row.ID
	}
	persist("user", "admitted question", false)
	refusedID := persist("user", "refused-marker text", true)
	persist("assistant", "admitted answer", false)
	persist("user", "refused-marker again", true)

	texts := func(got any) []string {
		t.Helper()
		out := make([]string, 0)
		for _, entry := range asMapSlice(t, got.(map[string]any)["messages"]) {
			out = append(out, entry["text"].(string))
		}
		slices.Sort(out)
		return out
	}
	assertTexts := func(t *testing.T, got any, want ...string) {
		t.Helper()
		slices.Sort(want)
		if have := texts(got); !slices.Equal(have, want) {
			t.Fatalf("result texts = %v, want %v", have, want)
		}
	}

	t.Run("get_messages latest", func(t *testing.T) {
		got, err := provider.execGetMessages(ctx, sess, getMessagesArgs{})
		if err != nil {
			t.Fatal(err)
		}
		assertTexts(t, got, "admitted question", "admitted answer")
	})
	t.Run("get_messages latest page size counts only admitted rows", func(t *testing.T) {
		got, err := provider.execGetMessages(ctx, sess, getMessagesArgs{Limit: intPtr(2)})
		if err != nil {
			t.Fatal(err)
		}
		assertTexts(t, got, "admitted question", "admitted answer")
	})
	t.Run("get_messages before", func(t *testing.T) {
		got, err := provider.execGetMessages(ctx, sess, getMessagesArgs{Before: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		if err != nil {
			t.Fatal(err)
		}
		assertTexts(t, got, "admitted question", "admitted answer")
	})
	t.Run("get_messages by id", func(t *testing.T) {
		got, err := provider.execGetMessages(ctx, sess, getMessagesArgs{MessageID: refusedID})
		if err != nil {
			t.Fatal(err)
		}
		assertTexts(t, got)
	})
	t.Run("search_messages", func(t *testing.T) {
		got, err := provider.execSearchMessages(ctx, sess, searchMessagesArgs{})
		if err != nil {
			t.Fatal(err)
		}
		assertTexts(t, got, "admitted question", "admitted answer")
	})
	t.Run("search_messages keyword", func(t *testing.T) {
		got, err := provider.execSearchMessages(ctx, sess, searchMessagesArgs{Keyword: "refused-marker"})
		if err != nil {
			t.Fatal(err)
		}
		assertTexts(t, got)
	})
}
