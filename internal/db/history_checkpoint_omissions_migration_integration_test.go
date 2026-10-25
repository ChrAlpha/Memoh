//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/team"
)

// 0172 lets a checkpoint stand for its history by an omission alone. Its down
// makes every checkpoint hold a summary again: one that only omits goes, with
// the checkpoints built on it, and its session falls back to its whole
// history; the rest lose the record of what they omit. Both run again
// without effect.
func TestHistoryCheckpointOmissionsMigration(t *testing.T) {
	ctx := context.Background()
	dsn := limitedOwnerDSN(t)
	migrateUpAll(t, dsn)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as the limited owner: %v", err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('memoh.team_id', $1, false)", team.DefaultTeamID); err != nil {
		t.Fatalf("set team context: %v", err)
	}
	exec := func(label, query string, args ...any) error {
		t.Helper()
		_, err := conn.Exec(ctx, query, args...)
		return err
	}
	must := func(label, query string, args ...any) {
		t.Helper()
		if err := exec(label, query, args...); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	userID, botID := uuid.NewString(), uuid.NewString()
	must("create user", `
		WITH created_user AS (INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, "history-omissions-"+userID)
	must("create bot", `INSERT INTO bots (id, team_id, owner_user_id, name) VALUES ($1, $2, $3, $4)`,
		botID, team.DefaultTeamID, userID, "history-omissions-"+botID)
	session := func() string {
		t.Helper()
		id := uuid.NewString()
		must("create session", `INSERT INTO bot_sessions (id, bot_id) VALUES ($1, $2)`, id, botID)
		return id
	}
	summary := func(sessionID string) string {
		t.Helper()
		id := uuid.NewString()
		must("create summary", `INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary, completed_at)
			VALUES ($1, $2, $3, 'ok', 'earlier', now())`, id, botID, sessionID)
		return id
	}
	checkpoint := func(sessionID, parentID string, omitted int, items string) string {
		t.Helper()
		id := uuid.NewString()
		var parent any
		if parentID != "" {
			parent = parentID
		}
		must("create checkpoint", `INSERT INTO bot_history_checkpoints (id, bot_id, session_id, parent_id, turn_position, turn_message_seq,
				omitted_count, omitted_first_at, omitted_last_at, items)
			VALUES ($1, $2, $3, $4, 3, 2, $5::bigint,
				CASE WHEN $5::bigint > 0 THEN now() - interval '1 day' END,
				CASE WHEN $5::bigint > 0 THEN now() END, $6::jsonb)`,
			id, botID, sessionID, parent, omitted, items)
		must("activate checkpoint", `UPDATE bot_sessions SET history_checkpoint_id = $1 WHERE id = $2`, id, sessionID)
		return id
	}

	// A step-over with a fold on it, and a fold that only carries what its
	// parent omits.
	stepped := session()
	steppedOver := checkpoint(stepped, "", 6, `[]`)
	foldedOn := checkpoint(stepped, steppedOver, 6, `[{"summary":"`+summary(stepped)+`"}]`)
	// A summary that also omits, and one that does not.
	summarized := session()
	both := checkpoint(summarized, "", 4, `[{"summary":"`+summary(summarized)+`"}]`)
	plain := session()
	summaryOnly := checkpoint(plain, "", 0, `[{"summary":"`+summary(plain)+`"}]`)
	if err := exec("neither", `INSERT INTO bot_history_checkpoints (bot_id, session_id, turn_position, turn_message_seq, items)
		VALUES ($1, $2, 1, 1, '[]')`, botID, plain); err == nil || !strings.Contains(err.Error(), "bot_history_checkpoints_stands_check") {
		t.Fatalf("a checkpoint with neither a summary nor an omission: %v", err)
	}

	migrateTo(t, dsn, 171)
	active := func(sessionID string) string {
		t.Helper()
		var id *string
		if err := conn.QueryRow(ctx, `SELECT history_checkpoint_id::text FROM bot_sessions WHERE id = $1`, sessionID).Scan(&id); err != nil {
			t.Fatalf("read active checkpoint: %v", err)
		}
		if id == nil {
			return ""
		}
		return *id
	}
	exists := func(id string) bool {
		t.Helper()
		var found bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bot_history_checkpoints WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("read checkpoint: %v", err)
		}
		return found
	}
	if exists(steppedOver) || exists(foldedOn) || active(stepped) != "" {
		t.Fatalf("down kept the step-over or what was built on it: active %q", active(stepped))
	}
	if !exists(both) || active(summarized) != both || !exists(summaryOnly) || active(plain) != summaryOnly {
		t.Fatal("down dropped a checkpoint that holds a summary")
	}
	if err := exec("omission alone after down", `INSERT INTO bot_history_checkpoints (bot_id, session_id, turn_position, turn_message_seq, items)
		VALUES ($1, $2, 1, 1, '[]')`, botID, plain); err == nil || !strings.Contains(err.Error(), "bot_history_checkpoints_summary_check") {
		t.Fatalf("a checkpoint without a summary after down: %v", err)
	}

	migrateUpAll(t, dsn)
	var omitted int64
	if err := conn.QueryRow(ctx, `SELECT omitted_count FROM bot_history_checkpoints WHERE id = $1`, both).Scan(&omitted); err != nil || omitted != 0 {
		t.Fatalf("omitted_count after up again = %d, %v; want 0", omitted, err)
	}
	checkpoint(stepped, "", 2, `[]`)
}
