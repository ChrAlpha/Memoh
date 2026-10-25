package botbackup

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
)

// A restored message keeps its original time, as the session events restored
// beside it do; one a backup records no time for is created as it is
// restored.
func TestPostgresRestoreHistoryKeepsMessageTimes(t *testing.T) {
	ctx := context.Background()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the history restore PostgreSQL integration")
	}
	if os.Getenv("TEST_POSTGRES_BOOTSTRAP_SCHEMA") == "1" {
		if err := dbtest.MigratePostgresUp(dsn); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}

	userID, botID := uuid.NewString(), uuid.NewString()
	exec(`WITH u AS (INSERT INTO users (id, username, is_active) VALUES ($1::uuid, $1::text, true) RETURNING id)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM u`, userID)
	exec(`INSERT INTO bots (id, owner_user_id, name) VALUES ($1::uuid, $2, $1::text)`, botID, userID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM bots WHERE id=$1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})

	sourceSession := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	sessions := []dbsqlc.ListSessionsByBotRow{{
		ID:          sourceSession,
		ChannelType: pgtype.Text{String: "local", Valid: true},
		Type:        "chat",
		SessionMode: "chat",
		RuntimeType: "model",
		Visibility:  "user",
		Title:       "Restored",
	}}
	start := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	var messages []dbsqlc.ListAllMessagesForBackupRow
	for position := int64(1); position <= 2; position++ {
		turnID := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		for seq, role := range []string{"user", "assistant"} {
			content, _ := json.Marshal(map[string]string{"content": role, "role": role})
			messages = append(messages, dbsqlc.ListAllMessagesForBackupRow{
				ID:             pgtype.UUID{Bytes: uuid.New(), Valid: true},
				SessionID:      sourceSession,
				Role:           role,
				Content:        content,
				Metadata:       []byte(`{}`),
				SessionMode:    "chat",
				RuntimeType:    "model",
				CreatedAt:      pgtype.Timestamptz{Time: start.Add(time.Duration(len(messages)) * time.Minute), Valid: true},
				TurnID:         turnID,
				TurnPosition:   pgtype.Int8{Int64: position, Valid: true},
				TurnMessageSeq: pgtype.Int8{Int64: int64(seq + 1), Valid: true},
				TurnVisible:    true,
			})
		}
	}
	// The newest answer comes from a backup that records no time.
	messages[len(messages)-1].CreatedAt = pgtype.Timestamptz{}
	entries := map[string]backupZipEntry{}
	for path, value := range map[string]any{"history/sessions.json": sessions, "history/messages.json": messages} {
		raw, err := marshalJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		entries[path] = backupZipEntry{data: raw}
	}

	svc := &Service{logger: slog.Default(), db: pool, queries: postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))}
	restoredAt := time.Now()
	if err := svc.restoreHistory(ctx, userID, botID, &importState{entries: entries, counts: map[Section]int{}}, false, false); err != nil {
		t.Fatalf("restore history: %v", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT message.created_at
		FROM bot_history_messages message
		JOIN bot_sessions session ON session.id = message.session_id
		WHERE session.bot_id = $1
		ORDER BY message.turn_position, message.turn_message_seq`, botID)
	if err != nil {
		t.Fatal(err)
	}
	var created []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		created = append(created, at)
	}
	rows.Close()
	if len(created) != len(messages) {
		t.Fatalf("restored %d messages, want %d", len(created), len(messages))
	}
	for i, message := range messages[:len(messages)-1] {
		if !created[i].Equal(message.CreatedAt.Time) {
			t.Fatalf("restored message %d created at %v, want its original %v", i, created[i], message.CreatedAt.Time)
		}
	}
	if last := created[len(created)-1]; last.Before(restoredAt.Add(-time.Minute)) {
		t.Fatalf("restored message without a time created at %v, want the restore time", last)
	}
}
