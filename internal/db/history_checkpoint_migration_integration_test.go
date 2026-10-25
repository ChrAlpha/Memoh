//go:build integration

package db_test

import (
	"context"
	"io/fs"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// historyCheckpointTables are the tables 0170 creates, each under FORCE row
// level security once it ran.
var historyCheckpointTables = []string{"bot_history_checkpoints", "bot_history_appeared_rows"}

// 0170 adds history checkpoints beside the compaction overlay, as an owner
// that FORCE row level security binds. Its down drops them. Both run again
// without effect, and the canonical init defines the same schema as the
// chain.
func TestHistoryCheckpointMigration(t *testing.T) {
	ctx := context.Background()
	dsn := limitedOwnerDSN(t)
	migrateUpAll(t, dsn)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as the limited owner: %v", err)
	}
	t.Cleanup(pool.Close)
	assertForcedRLS(t, ctx, pool, historyCheckpointTables...)
	chain := historyCheckpointSchema(t, ctx, pool)

	migrations := postgresMigrationsFS(t)
	up, err := fs.ReadFile(migrations, "0170_history_checkpoints.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("re-apply 0170 up: %v", err)
	}
	if got := historyCheckpointSchema(t, ctx, pool); !slices.Equal(got, chain) {
		t.Fatalf("0170 up again defines\n%q\nthe chain defines\n%q", got, chain)
	}

	assertDropped := func() {
		t.Helper()
		var left int
		if err := pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM information_schema.tables
			   WHERE table_schema = 'public' AND table_name IN ('bot_history_checkpoints', 'bot_history_appeared_rows'))
			  + (SELECT count(*) FROM information_schema.columns
			     WHERE table_schema = 'public' AND table_name = 'bot_sessions' AND column_name = 'history_checkpoint_id')
			  + (SELECT count(*) FROM pg_proc
			     WHERE pronamespace = 'public'::regnamespace
			       AND proname IN ('memoh_history_checkpoint_kept', 'memoh_rewind_history_checkpoint',
			         'memoh_history_checkpoint_hides', 'memoh_history_row_appeared'))
		`).Scan(&left); err != nil {
			t.Fatalf("inspect the schema after down: %v", err)
		}
		if left != 0 {
			t.Errorf("after down %d pieces of the history checkpoints are left", left)
		}
	}
	migrateTo(t, dsn, 169)
	assertDropped()
	down, err := fs.ReadFile(migrations, "0170_history_checkpoints.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("re-apply 0170 down: %v", err)
	}
	assertDropped()

	stepUp(t, dsn, 1)
	assertForcedRLS(t, ctx, pool, historyCheckpointTables...)
	if got := historyCheckpointSchema(t, ctx, pool); !slices.Equal(got, chain) {
		t.Fatalf("0170 after its down defines\n%q\nthe chain defines\n%q", got, chain)
	}

	t.Run("canonical init defines the same history checkpoints", func(t *testing.T) {
		dsn := teamMigrationDSN(t)
		canonical := resetToEmpty(t)
		applyCanonicalInitOnly(t, dsn)
		if got := historyCheckpointSchema(t, ctx, canonical); !slices.Equal(got, chain) {
			t.Fatalf("canonical init defines\n%q\nthe chain defines\n%q", got, chain)
		}
	})
}

// historyCheckpointSchema is how a database defines history checkpoints and
// the tables they stand beside: the checkpoint tables and the tables the
// compaction overlay lives in, with their columns, constraints and indexes,
// the checkpoint policies, the functions that move a session's checkpoint or
// record a row below it, the triggers that call them, and the visible-history
// view the rest reads.
func historyCheckpointSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	var definitions []string
	if err := pool.QueryRow(ctx, `
		WITH tables AS (
		  SELECT name, ('public.' || name)::regclass AS oid
		  FROM unnest(ARRAY[
		    'bot_history_checkpoints', 'bot_history_appeared_rows', 'bot_sessions', 'bot_history_messages',
		    'bot_history_message_compacts'
		  ]) AS name
		)
		SELECT ARRAY(
		  SELECT a.attrelid::regclass::text || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
		    || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
		    || COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '')
		  FROM pg_attribute a
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		  WHERE a.attrelid IN (SELECT oid FROM tables)
		    AND a.attnum > 0
		    AND NOT a.attisdropped
		  ORDER BY 1
		) || ARRAY(
		  -- Older migrations add references to tables under FORCE row level
		  -- security NOT VALID where the canonical init declares them plainly;
		  -- whether a reference was validated is not what is compared here.
		  SELECT c.conrelid::regclass::text || '.' || c.conname || ' '
		    || regexp_replace(pg_get_constraintdef(c.oid), ' NOT VALID$', '')
		  FROM pg_constraint c
		  WHERE c.conrelid IN (SELECT oid FROM tables)
		  ORDER BY 1
		) || ARRAY(
		  SELECT indexdef
		  FROM pg_indexes
		  WHERE schemaname = 'public'
		    AND tablename IN (SELECT name FROM tables)
		  ORDER BY indexname
		) || ARRAY(
		  SELECT policyname || ' ' || cmd || ' ' || COALESCE(qual, '') || ' ' || COALESCE(with_check, '')
		  FROM pg_policies
		  WHERE schemaname = 'public' AND tablename IN ('bot_history_checkpoints', 'bot_history_appeared_rows')
		  ORDER BY policyname
		) || ARRAY(
		  SELECT pg_get_functiondef(p.oid)
		  FROM pg_proc p
		  WHERE p.pronamespace = 'public'::regnamespace
		    AND p.proname IN ('memoh_history_checkpoint_kept', 'memoh_rewind_history_checkpoint',
		      'memoh_history_checkpoint_hides', 'memoh_history_row_appeared')
		  ORDER BY p.proname
		) || ARRAY(
		  SELECT pg_get_triggerdef(tg.oid)
		  FROM pg_trigger tg
		  WHERE tg.tgrelid = 'public.bot_history_messages'::regclass
		    AND NOT tg.tgisinternal
		  ORDER BY tg.tgname
		) || ARRAY[pg_get_viewdef('public.bot_visible_history_messages'::regclass)]
	`).Scan(&definitions); err != nil {
		t.Fatalf("read the history checkpoint schema: %v", err)
	}
	return definitions
}
