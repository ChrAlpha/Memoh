package db

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// A compaction pass reads its candidates while the session may still be running
// a turn: that run links outbound assets and appends steps to its own rows, and
// an asset landing on a claimed row invalidates every summary of the session.
// The run's turns are therefore never candidates until the run is terminal.
func TestUncompactedMessagesExcludeTheActiveRunTurn(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	userID, botID, sessionID := uuid.New(), uuid.New(), uuid.New()
	name := "active-run-" + uuid.NewString()
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", botID)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("fixture %q: %v", query, err)
		}
	}
	mustExec(`WITH u AS (INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM u`, userID, name)
	mustExec(`INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, botID, userID, name)
	mustExec(`INSERT INTO bot_sessions (id, bot_id, channel_type) VALUES ($1, $2, 'local')`, sessionID, botID)

	finishedTurn, activeTurn, injectedTurn, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var finished, active []string
	insert := func(turn uuid.UUID, position, seq int, role string) string {
		id := uuid.New()
		mustExec(`INSERT INTO bot_history_messages (
				id, bot_id, session_id, role, content, metadata, turn_id, turn_position, turn_message_seq, turn_visible
			) VALUES ($1, $2, $3, $4, jsonb_build_object('role', $4::text, 'content', 'text'), '{}'::jsonb, $5, $6, $7, true)`,
			id, botID, sessionID, role, turn, position, seq)
		return id.String()
	}
	finished = append(finished, insert(finishedTurn, 1, 1, "user"), insert(finishedTurn, 1, 2, "assistant"))
	active = append(active, insert(activeTurn, 2, 1, "user"), insert(activeTurn, 2, 2, "assistant"))
	// A message steered or injected into the run opens a later turn the run
	// keeps writing under.
	active = append(active, insert(injectedTurn, 3, 1, "user"), insert(injectedTurn, 3, 2, "assistant"))
	mustExec(`INSERT INTO session_runs (
			run_id, bot_id, session_id, invocation_id, turn_id, turn_position, state, input_json, input_fingerprint
		) VALUES ($1, $2, $3, $4, $5, 2, 'running', '{}'::jsonb, 'fingerprint')`,
		runID, botID, sessionID, uuid.NewString(), activeTurn)

	queries := sqlc.New(pool)
	session := pgtype.UUID{Bytes: sessionID, Valid: true}
	assertCandidates := func(state string, want []string) {
		t.Helper()
		bounded, err := queries.ListUncompactedMessagesBySessionWithinBytes(ctx, sqlc.ListUncompactedMessagesBySessionWithinBytesParams{SessionID: session, MaxBytes: 8 << 20})
		if err != nil {
			t.Fatalf("%s: bounded candidates: %v", state, err)
		}
		got := make([]string, 0, len(bounded))
		for _, row := range bounded {
			got = append(got, uuid.UUID(row.ID.Bytes).String())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: bounded candidates = %v, want %v", state, got, want)
		}
		if bounded[0].CandidateCount != int64(len(want)) {
			t.Fatalf("%s: candidate count = %d, want %d", state, bounded[0].CandidateCount, len(want))
		}
		measure, err := queries.MeasureUncompactedMessagesBySession(ctx, session)
		if err != nil || measure.CandidateCount != int64(len(want)) {
			t.Fatalf("%s: measured candidates = %d, %v; want %d", state, measure.CandidateCount, err, len(want))
		}
		all, err := queries.ListUncompactedMessagesBySession(ctx, session)
		if err != nil || len(all) != len(want) {
			t.Fatalf("%s: unbounded candidates = %d, %v; want %d", state, len(all), err, len(want))
		}
	}

	for _, state := range []string{"accepted", "running", "waiting_decision"} {
		mustExec(`UPDATE session_runs SET state = $2 WHERE run_id = $1`, runID, state)
		assertCandidates(state, finished)
	}
	mustExec(`UPDATE session_runs SET state = 'finishing', proposed_terminal_state = 'completed', finish_proposed_at = now() WHERE run_id = $1`, runID)
	assertCandidates("finishing", finished)
	mustExec(`UPDATE session_runs SET state = 'completed' WHERE run_id = $1`, runID)
	assertCandidates("completed", append(slices.Clone(finished), active...))
}
