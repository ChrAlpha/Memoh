package message

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// A history load starts at the oldest visible row no usable summary of the
// session's current epoch replaces; every older row is summarized.
func TestPostgresSessionUnsummarizedFrom(t *testing.T) {
	ctx := context.Background()
	tx := beginPostgresMessageTestTx(t, ctx)
	setupPostgresMessageTestFixtures(t, ctx, tx)
	queries := dbsqlc.New(tx)
	sessionID, err := dbpkg.ParseUUID(postgresMessageTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }
	unsummarizedFrom := func(horizon time.Time) pgtype.Timestamptz {
		t.Helper()
		got, err := queries.GetSessionUnsummarizedFrom(ctx, dbsqlc.GetSessionUnsummarizedFromParams{
			SessionID: sessionID, Horizon: pgtype.Timestamptz{Time: horizon, Valid: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	expect := func(step string, horizon time.Time, want time.Time) {
		t.Helper()
		if got := unsummarizedFrom(horizon); !got.Valid || !got.Time.Equal(want) {
			t.Fatalf("%s: unsummarized from %v, want %v", step, got, want)
		}
	}
	open := time.Time{}

	if got := unsummarizedFrom(open); got.Valid {
		t.Fatalf("a session without rows starts at %v, want none", got.Time)
	}

	rows := make([]string, 6)
	for i := range rows {
		rows[i] = insertUnsummarizedTestRow(t, ctx, tx, at(i+1), `{}`, true)
	}
	expect("nothing compacted", open, at(1))

	summary := insertUnsummarizedTestCompact(t, ctx, tx, "ok", "rolling summary")
	setUnsummarizedTestCompact(t, ctx, tx, summary, rows[0], rows[1], rows[2])
	expect("the oldest rows summarized", open, at(4))

	// Rows a load never reads do not hold the start back.
	insertUnsummarizedTestRow(t, ctx, tx, at(0), `{}`, false)
	insertUnsummarizedTestRow(t, ctx, tx, at(0), `{"trigger_mode":"passive_sync"}`, true)
	expect("hidden and passive rows", open, at(4))

	// A summary that is not usable does not replace its rows.
	for _, unusable := range []struct{ status, summary string }{
		{"error", "rolling summary"}, {"pending", ""}, {"ok", "  \n"},
	} {
		compact := insertUnsummarizedTestCompact(t, ctx, tx, unusable.status, unusable.summary)
		setUnsummarizedTestCompact(t, ctx, tx, compact, rows[1])
		expect(unusable.status+" compact", open, at(2))
		setUnsummarizedTestCompact(t, ctx, tx, summary, rows[1])
	}

	// Every row summarized: the newest one places the summary.
	setUnsummarizedTestCompact(t, ctx, tx, summary, rows[3], rows[4], rows[5])
	expect("every row summarized", open, at(6))

	// Rows before the horizon are not the session's history.
	failed := insertUnsummarizedTestCompact(t, ctx, tx, "error", "")
	setUnsummarizedTestCompact(t, ctx, tx, failed, rows[0])
	expect("an unusable row before the horizon", at(2), at(6))
	expect("an unusable row from the horizon on", at(1), at(1))
	setUnsummarizedTestCompact(t, ctx, tx, summary, rows[0])

	// Rewriting history moves the session past every summary it had.
	if _, err := tx.Exec(ctx, `UPDATE bot_sessions SET compaction_epoch = compaction_epoch + 1 WHERE id = $1`, postgresMessageTestSessionID); err != nil {
		t.Fatal(err)
	}
	expect("an earlier epoch's summary", open, at(1))

	if _, err := tx.Exec(ctx, "SELECT set_config('memoh.team_id', $1, true)", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if got := unsummarizedFrom(open); got.Valid {
		t.Fatalf("another team's session starts at %v, want none", got.Time)
	}
}

func insertUnsummarizedTestRow(t *testing.T, ctx context.Context, tx pgx.Tx, created time.Time, metadata string, visible bool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO bot_history_messages
		(id, bot_id, session_id, role, content, metadata, created_at, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, $3, 'user', '{"role":"user","content":"row"}'::jsonb, $4::jsonb, $5, gen_random_uuid(), 1, 1, $6)`,
		id, postgresMessageTestBotID, postgresMessageTestSessionID, metadata, created, visible); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertUnsummarizedTestCompact(t *testing.T, ctx context.Context, tx pgx.Tx, status, summary string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO bot_history_message_compacts (id, bot_id, session_id, status, summary)
		VALUES ($1, $2, $3, $4, $5)`, id, postgresMessageTestBotID, postgresMessageTestSessionID, status, summary); err != nil {
		t.Fatal(err)
	}
	return id
}

func setUnsummarizedTestCompact(t *testing.T, ctx context.Context, tx pgx.Tx, compactID string, rowIDs ...string) {
	t.Helper()
	if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET compact_id = $1 WHERE id = ANY($2::uuid[])`, compactID, rowIDs); err != nil {
		t.Fatal(err)
	}
}
