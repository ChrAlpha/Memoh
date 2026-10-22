package dbtest

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	dbpkg "github.com/felinics/memoh/internal/db"
)

var (
	migrationOnce sync.Once
	migrationErr  error
)

// OpenPostgres opens a pool on the database TEST_POSTGRES_DSN names and skips
// the test when it is unset. With TEST_POSTGRES_BOOTSTRAP_SCHEMA=1 it migrates
// the database first, once per test binary.
func OpenPostgres(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the PostgreSQL integration")
	}
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if os.Getenv("TEST_POSTGRES_BOOTSTRAP_SCHEMA") == "1" {
		migrationOnce.Do(func() {
			migrationErr = MigratePostgresUp(dsn)
		})
		if migrationErr != nil {
			t.Fatalf("migrate PostgreSQL test database: %v", migrationErr)
		}
	}
	return pool
}
