package postgresstore

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// These writes invalidate the compaction artifacts that cover the rows they
// change, and with them any rollup above those artifacts. Each one then
// releases the valid parents of the rollups it invalidated. The release is one
// query, run in the same transaction as the write.

func (q *Queries) ReplaceHistoryTurn(ctx context.Context, arg dbsqlc.ReplaceHistoryTurnParams) (dbsqlc.ReplaceHistoryTurnRow, error) {
	row, err := q.Queries.ReplaceHistoryTurn(ctx, arg)
	if err != nil {
		return dbsqlc.ReplaceHistoryTurnRow{}, err
	}
	if err := q.ReleaseParentsOfInvalidatedRollups(ctx); err != nil {
		return dbsqlc.ReplaceHistoryTurnRow{}, err
	}
	return row, nil
}

func (q *Queries) HideMessagesByHistoryTurn(ctx context.Context, turnID pgtype.UUID) error {
	if err := q.Queries.HideMessagesByHistoryTurn(ctx, turnID); err != nil {
		return err
	}
	return q.ReleaseParentsOfInvalidatedRollups(ctx)
}

func (q *Queries) DeleteMessagesByIDs(ctx context.Context, ids []pgtype.UUID) error {
	if err := q.Queries.DeleteMessagesByIDs(ctx, ids); err != nil {
		return err
	}
	return q.ReleaseParentsOfInvalidatedRollups(ctx)
}
