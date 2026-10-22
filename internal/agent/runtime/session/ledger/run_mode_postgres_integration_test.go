package ledger_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func int64Ptr(v int64) *int64 { return &v }

// The ledger records the mode a run is admitted in and, for a discuss run, the
// position its batch ends at. A run admitted without either is a chat run.
func TestPostgresAdmitRecordsRunModeAndDiscussCover(t *testing.T) {
	ctx := t.Context()
	pool := openLedgerResetPostgres(t, ctx)
	botID, sessionID := createLedgerResetFixture(t, ctx, pool)
	runs := ledger.NewPostgres(dbsqlc.New(pool), pool)

	cases := []struct {
		name      string
		mode      string
		cover     *int64
		wantMode  string
		wantCover *int64
	}{
		{name: "unset mode is a chat run", wantMode: sessionmode.Chat},
		{name: "chat", mode: sessionmode.Chat, wantMode: sessionmode.Chat},
		{name: "schedule", mode: sessionmode.Schedule, wantMode: sessionmode.Schedule},
		{name: "subagent", mode: sessionmode.Subagent, wantMode: sessionmode.Subagent},
		{name: "acp agent", mode: sessionmode.ACPAgent, wantMode: sessionmode.ACPAgent},
		{name: "discuss", mode: sessionmode.Discuss, cover: int64Ptr(7), wantMode: sessionmode.Discuss, wantCover: int64Ptr(7)},
	}
	for _, tc := range cases {
		params := admitParams(botID, sessionID, tc.name, nil)
		params.Mode, params.DiscussCoverPosition = tc.mode, tc.cover
		run, created, err := runs.Admit(ctx, params)
		if err != nil || !created {
			t.Fatalf("%s: admit = %v created=%v", tc.name, err, created)
		}
		var mode string
		var cover *int64
		if err := pool.QueryRow(ctx, "SELECT mode, discuss_cover_position FROM session_runs WHERE run_id=$1", run.RunID).Scan(&mode, &cover); err != nil {
			t.Fatal(err)
		}
		if mode != tc.wantMode || (cover == nil) != (tc.wantCover == nil) || (cover != nil && *cover != *tc.wantCover) {
			t.Fatalf("%s: stored mode=%q cover=%v, want %q %v", tc.name, mode, cover, tc.wantMode, tc.wantCover)
		}
		// One run is active per session; end it so the next case can be admitted.
		if _, err := pool.Exec(ctx, "UPDATE session_runs SET state='failed' WHERE run_id=$1", run.RunID); err != nil {
			t.Fatal(err)
		}
	}
}

// A discuss run covers a position and only a discuss run does; a mode outside
// the session modes is refused. The table states both, so a writer that forgets
// the cover position, or invents a mode, fails the admission instead of leaving
// a run the cursor cannot see.
func TestPostgresAdmitRefusesInconsistentRunMode(t *testing.T) {
	ctx := t.Context()
	pool := openLedgerResetPostgres(t, ctx)
	botID, sessionID := createLedgerResetFixture(t, ctx, pool)
	runs := ledger.NewPostgres(dbsqlc.New(pool), pool)

	cases := []struct {
		name           string
		mode           string
		cover          *int64
		wantConstraint string
	}{
		{name: "discuss without a cover position", mode: sessionmode.Discuss, wantConstraint: "session_runs_discuss_cover_pairing_check"},
		{name: "chat with a cover position", mode: sessionmode.Chat, cover: int64Ptr(3), wantConstraint: "session_runs_discuss_cover_pairing_check"},
		{name: "cover position of zero", mode: sessionmode.Discuss, cover: int64Ptr(0), wantConstraint: "session_runs_discuss_cover_position_check"},
		{name: "unknown mode", mode: "heartbeat", wantConstraint: "session_runs_mode_check"},
	}
	for _, tc := range cases {
		params := admitParams(botID, sessionID, tc.name, nil)
		params.Mode, params.DiscussCoverPosition = tc.mode, tc.cover
		_, _, err := runs.Admit(ctx, params)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.wantConstraint {
			t.Fatalf("%s: admit = %v, want a violation of %s", tc.name, err, tc.wantConstraint)
		}
	}
}
