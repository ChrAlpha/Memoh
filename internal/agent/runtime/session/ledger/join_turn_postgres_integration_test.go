package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func nextTurnPosition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sessionID string) int64 {
	t.Helper()
	var next int64
	if err := pool.QueryRow(ctx, "SELECT next_turn_position FROM bot_sessions WHERE id=$1", sessionID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	return next
}

func admitParams(botID, sessionID, invocation string, join *ledger.Turn) ledger.AdmitParams {
	turnID := uuid.NewString()
	if join != nil {
		turnID = join.ID
	}
	return ledger.AdmitParams{
		RunID: uuid.NewString(), BotID: botID, SessionID: sessionID, InvocationID: invocation,
		TurnID: turnID, JoinTurn: join, Input: []byte(`{}`), InputFingerprint: invocation,
	}
}

// A run that answers a request row which already has a turn takes that turn and
// draws nothing from the session counter; a replay of it changes nothing.
func TestPostgresAdmitJoinsExistingTurnWithoutDrawingAPosition(t *testing.T) {
	ctx := t.Context()
	pool := openLedgerResetPostgres(t, ctx)
	botID, sessionID := createLedgerResetFixture(t, ctx, pool)
	runs := ledger.NewPostgres(dbsqlc.New(pool), pool)

	first, created, err := runs.Admit(ctx, admitParams(botID, sessionID, "first", nil))
	if err != nil || !created {
		t.Fatalf("first admit: %v %v", created, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE session_runs SET state='failed' WHERE run_id=$1", first.RunID); err != nil {
		t.Fatal(err)
	}
	counter := nextTurnPosition(t, ctx, pool, sessionID)

	join := &ledger.Turn{ID: first.TurnID, Position: first.TurnPosition}
	joined, created, err := runs.Admit(ctx, admitParams(botID, sessionID, "second", join))
	if err != nil || !created {
		t.Fatalf("joining admit: %v %v", created, err)
	}
	if joined.TurnID != first.TurnID || joined.TurnPosition != first.TurnPosition {
		t.Fatalf("joined run turn = %s/%d, want %s/%d", joined.TurnID, joined.TurnPosition, first.TurnID, first.TurnPosition)
	}
	if got := nextTurnPosition(t, ctx, pool, sessionID); got != counter {
		t.Fatalf("counter moved by a joining admission: %d -> %d", counter, got)
	}
	if _, err := pool.Exec(ctx, "UPDATE session_runs SET state='failed' WHERE run_id=$1", joined.RunID); err != nil {
		t.Fatal(err)
	}

	replay, created, err := runs.Admit(ctx, admitParams(botID, sessionID, "second", join))
	if err != nil || created || replay.RunID != joined.RunID {
		t.Fatalf("replay: created=%v run=%s err=%v", created, replay.RunID, err)
	}
	if got := nextTurnPosition(t, ctx, pool, sessionID); got != counter {
		t.Fatalf("counter moved by a replay: %d -> %d", counter, got)
	}

	fresh, created, err := runs.Admit(ctx, admitParams(botID, sessionID, "third", nil))
	if err != nil || !created || fresh.TurnPosition != counter || fresh.TurnID == first.TurnID {
		t.Fatalf("new turn after a join: %+v created=%v err=%v, want position %d", fresh, created, err, counter)
	}
}

// A continuation shares the interrupted run's turn, so neither supersedes the
// other: admission retires the source's intent, only the latest interruption is
// owed, and a later user turn still supersedes it.
func TestPostgresContinuationSharesTurnAndOnlyLatestInterruptionIsOwed(t *testing.T) {
	ctx := t.Context()
	pool := openLedgerResetPostgres(t, ctx)
	botID, sessionID := createLedgerResetFixture(t, ctx, pool)
	queries := dbsqlc.New(pool)
	runs := ledger.NewPostgres(queries, pool)
	interrupt := func(runID string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE session_runs SET state='lost', error_code=$2,
			input_json = '{"resume":{"version":1,"chat_id":"c"}}'::jsonb WHERE run_id=$1`, runID, sessionruntime.RunErrorInterrupted); err != nil {
			t.Fatal(err)
		}
	}
	owed := func() []string {
		t.Helper()
		rows, err := queries.ListInterruptedSessionRuns(ctx, db.ParseUUIDOrEmpty(uuid.Nil.String()))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, row := range rows {
			if row.SessionID.String() == sessionID {
				ids = append(ids, row.RunID.String())
			}
		}
		return ids
	}

	original, _, err := runs.Admit(ctx, admitParams(botID, sessionID, "original", nil))
	if err != nil {
		t.Fatal(err)
	}
	interrupt(original.RunID)
	join := &ledger.Turn{ID: original.TurnID, Position: original.TurnPosition}
	p := admitParams(botID, sessionID, "resume:"+original.RunID, join)
	p.ResumeRunID = original.RunID
	resumed, created, err := runs.Admit(ctx, p)
	if err != nil || !created || resumed.TurnPosition != original.TurnPosition {
		t.Fatalf("continuation: %+v created=%v err=%v", resumed, created, err)
	}
	interrupt(resumed.RunID)
	if got := owed(); len(got) != 1 || got[0] != resumed.RunID {
		t.Fatalf("owed after the second interruption = %v, want only %s", got, resumed.RunID)
	}

	if _, _, err := runs.Admit(ctx, admitParams(botID, sessionID, "user-turn", nil)); err != nil {
		t.Fatal(err)
	}
	if got := owed(); len(got) != 0 {
		t.Fatalf("a later user turn left %v owed", got)
	}
	retired, err := queries.RetireSupersededInterruptedSessionRuns(ctx)
	if err != nil || retired < 1 {
		t.Fatalf("superseded intent retired = %d, %v", retired, err)
	}
}
