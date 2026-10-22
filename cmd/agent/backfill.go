package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// runtimeOwnerBatch is how many sessions one statement fills, so no statement
// holds the table's rows for long.
const runtimeOwnerBatch = 500

// runtimeOwnerAdopter is the one statement the runtime-owner backfill repeats.
type runtimeOwnerAdopter interface {
	AdoptSessionRuntimeOwnersFromMetadata(ctx context.Context, batchSize int32) (int64, error)
}

// adoptRuntimeOwners fills the owner column of every session that has none and
// whose metadata names a member of the team, one batch at a time, and returns
// how many it filled. A filled session no longer matches, so it stops when a
// batch is empty and running it again changes nothing.
func adoptRuntimeOwners(ctx context.Context, queries runtimeOwnerAdopter, batchSize int32) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		filled, err := queries.AdoptSessionRuntimeOwnersFromMetadata(ctx, batchSize)
		if err != nil {
			return total, fmt.Errorf("adopt runtime owners: %w", err)
		}
		if filled == 0 {
			return total, nil
		}
		total += filled
	}
}

// runBackfillCommand runs a one-off pass over data written before a column
// existed. It is not part of the migrations: those hold locks for a whole file,
// and this pass is only as long as the data is.
func runBackfillCommand(args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "runtime-owner" {
		return errors.New("usage: memoh-server backfill runtime-owner")
	}
	cfg, err := provideConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	total, err := adoptRuntimeOwners(ctx, dbsqlc.New(pool), runtimeOwnerBatch)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "filled the runtime owner of %d sessions\n", total)
	return err
}

func runBackfill(args []string) {
	if err := runBackfillCommand(args, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "backfill: %v\n", err)
		os.Exit(1)
	}
}
