package agenttest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateSessionFixture mirrors the ledger integration fixtures: random
// identities relying on the default team, cleaned up by deleting the bot
// (session_runs and bot_sessions cascade) and the user. It returns the bot and
// session ids.
func CreateSessionFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	userID := uuid.New()
	botID := uuid.New()
	sessionID := uuid.New()
	name := "turn-admission-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active)
			VALUES ($1, $2, true)
			RETURNING id
		)
		INSERT INTO team_members (user_id, role)
		SELECT id, 'admin' FROM created_user
	`, userID, name); err != nil {
		t.Fatalf("create user fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)
	`, botID, userID, name); err != nil {
		t.Fatalf("create bot fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type)
		VALUES ($1, $2, 'telegram', 'model')
	`, sessionID, botID); err != nil {
		t.Fatalf("create session fixture: %v", err)
	}
	cleanupCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM bots WHERE id = $1", botID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", userID)
	})
	t.Cleanup(func() {
		if !t.Failed() {
			AssertRunRowsInRunTurns(t, cleanupCtx, pool, sessionID.String())
		}
	})
	return botID.String(), sessionID.String()
}

// AssertRunRowsInRunTurns checks the invariant that a run's turn is the turn of
// every row it wrote: each row carrying a run id sits under that run's turn, or
// under a turn whose request row the same run wrote (a steer opens its own).
// Every entry point that runs through CreateSessionFixture is held to it.
func AssertRunRowsInRunTurns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sessionID string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT m.id::text, m.role, r.invocation_id, m.turn_id::text, r.turn_id::text
		FROM bot_history_messages m
		JOIN session_runs r ON r.team_id = m.team_id AND r.run_id = m.run_id
		WHERE m.session_id = $1
		  AND m.turn_id IS DISTINCT FROM r.turn_id
		  AND NOT EXISTS (
		    SELECT 1 FROM bot_history_messages u
		    WHERE u.team_id = m.team_id AND u.run_id = m.run_id AND u.turn_id = m.turn_id AND u.role = 'user'
		  )
	`, sessionID)
	if err != nil {
		t.Errorf("check run rows against run turns: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, role, invocation, rowTurn, runTurn string
		if err := rows.Scan(&id, &role, &invocation, &rowTurn, &runTurn); err != nil {
			t.Errorf("scan: %v", err)
			return
		}
		t.Errorf("%s row %s of run %q sits in turn %s, not its run's turn %s", role, id, invocation, rowTurn, runTurn)
	}
}
