package db

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCompactionTargetPercentMigrationFiles(t *testing.T) {
	baseline := readEmbeddedMigration(t, "postgres/migrations/0001_init.up.sql")
	if !strings.Contains(baseline, "compaction_target_percent INTEGER") || strings.Contains(baseline, "compaction_ratio INTEGER") {
		t.Fatal("canonical schema must contain only the nullable compaction target percent")
	}
	up := readEmbeddedMigration(t, "postgres/migrations/0124_compaction_target_percent.up.sql")
	if !strings.Contains(up, "100 - compaction_ratio") || !strings.Contains(up, "compaction_threshold > 0") {
		t.Fatal("up migration must map legacy manual ratios to target percentages")
	}
	down := readEmbeddedMigration(t, "postgres/migrations/0124_compaction_target_percent.down.sql")
	if !strings.Contains(down, "GREATEST(1, LEAST(100, 100 - compaction_target_percent))") {
		t.Fatal("down migration must restore a clamped legacy ratio")
	}
}

func TestCompactionTargetPercentMigrationPostgresPath(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	schema := "compaction_target_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+quotedSchema); err != nil {
		t.Fatalf("set search path: %v", err)
	}
	if _, err := tx.Exec(ctx, `
CREATE TABLE bots (
  id INTEGER PRIMARY KEY,
  compaction_threshold INTEGER NOT NULL DEFAULT 0,
  compaction_ratio INTEGER NOT NULL DEFAULT 80
);
INSERT INTO bots (id, compaction_threshold, compaction_ratio)
VALUES (1, 0, 80), (2, 100000, 80), (3, 100000, 1);
`); err != nil {
		t.Fatalf("create legacy fixture: %v", err)
	}

	up := readEmbeddedMigration(t, "postgres/migrations/0124_compaction_target_percent.up.sql")
	down := readEmbeddedMigration(t, "postgres/migrations/0124_compaction_target_percent.down.sql")
	if _, err := tx.Exec(ctx, up); err != nil {
		t.Fatalf("apply 0124 up: %v", err)
	}
	if _, err := tx.Exec(ctx, up); err != nil {
		t.Fatalf("reapply 0124 up: %v", err)
	}
	assertColumnExists(t, ctx, tx, schema, "bots", "compaction_ratio", false)
	assertColumnExists(t, ctx, tx, schema, "bots", "compaction_target_percent", true)
	assertNullableInt(t, ctx, tx, 1, nil)
	assertNullableInt(t, ctx, tx, 2, intPointer(20))
	assertNullableInt(t, ctx, tx, 3, intPointer(99))

	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("apply 0124 down: %v", err)
	}
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("reapply 0124 down: %v", err)
	}
	assertColumnExists(t, ctx, tx, schema, "bots", "compaction_target_percent", false)
	assertColumnExists(t, ctx, tx, schema, "bots", "compaction_ratio", true)
	assertInt(t, ctx, tx, 1, 80)
	assertInt(t, ctx, tx, 2, 80)
	assertInt(t, ctx, tx, 3, 1)

	if _, err := tx.Exec(ctx, up); err != nil {
		t.Fatalf("apply 0124 up after down: %v", err)
	}
	assertNullableInt(t, ctx, tx, 1, nil)
	assertNullableInt(t, ctx, tx, 2, intPointer(20))
	assertNullableInt(t, ctx, tx, 3, intPointer(99))
}

func assertNullableInt(t *testing.T, ctx context.Context, tx pgx.Tx, id int, want *int) {
	t.Helper()
	var got *int
	if err := tx.QueryRow(ctx, "SELECT compaction_target_percent FROM bots WHERE id = $1", id).Scan(&got); err != nil {
		t.Fatalf("read target for bot %d: %v", id, err)
	}
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("target for bot %d = %v, want %v", id, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("target for bot %d = %d, want %d", id, *got, *want)
	}
}

func assertInt(t *testing.T, ctx context.Context, tx pgx.Tx, id, want int) {
	t.Helper()
	var got int
	if err := tx.QueryRow(ctx, "SELECT compaction_ratio FROM bots WHERE id = $1", id).Scan(&got); err != nil {
		t.Fatalf("read ratio for bot %d: %v", id, err)
	}
	if got != want {
		t.Fatalf("ratio for bot %d = %d, want %d", id, got, want)
	}
}

func intPointer(value int) *int {
	return &value
}
