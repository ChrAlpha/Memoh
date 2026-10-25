package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

const meterName = "github.com/felinics/memoh/internal/chat/checkpoint"

// activeReads bounds how often Active reads the session again after another
// writer moved its checkpoint while Active was replacing one that no longer
// held.
const activeReads = 3

// Why a checkpoint no longer holds.
const (
	reasonMissingRow     = "missing_row"
	reasonMissingSummary = "missing_summary"
	reasonRowAppeared    = "row_appeared"
)

// Store reads and writes the history checkpoints of sessions.
type Store struct {
	queries dbstore.Queries
	logger  *slog.Logger
}

// NewStore creates a Store.
func NewStore(log *slog.Logger, queries dbstore.Queries) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{queries: queries, logger: log.With(slog.String("service", "chat/checkpoint"))}
}

// Active is the session's active checkpoint as a request renders it, with
// the text of its summaries: the zero Checkpoint when the session has none.
//
// A checkpoint holds while every row it keeps is a context row of the
// session, every summary it holds is usable, and no context row appeared at
// or before its position after it was made. Removing rows moves the session
// back as they are removed. A row that appears is only recorded as it is
// written (memoh_history_row_appeared): its writer cannot take the session's
// row lock without waiting on a transaction that waits on it. This read
// moves the session back for it, and for any change made without either.
// One that no longer holds is replaced for the session by the newest
// checkpoint on its chain that holds on its own, and the repair is logged
// and counted. That member is right for the history: no checkpoint keeps a
// row its parent folded.
func (s *Store) Active(ctx context.Context, sessionID string) (Checkpoint, error) {
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return Checkpoint{}, err
	}
	for range activeReads {
		row, err := s.queries.GetActiveHistoryCheckpoint(ctx, pgSessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Checkpoint{}, nil
		}
		if err != nil {
			return Checkpoint{}, fmt.Errorf("load active history checkpoint: %w", err)
		}
		held, v, err := holding(ctx, s.queries, row)
		if err != nil {
			return Checkpoint{}, err
		}
		if v.reason != "" {
			moved, err := s.queries.SetHistoryCheckpoint(ctx, sqlc.SetHistoryCheckpointParams{
				CheckpointID:         dbpkg.ParseUUIDOrEmpty(held.ID),
				SessionID:            pgSessionID,
				ExpectedCheckpointID: row.ID,
			})
			if err != nil {
				return Checkpoint{}, fmt.Errorf("repair history checkpoint: %w", err)
			}
			if moved == 0 {
				continue
			}
			s.logger.WarnContext(ctx, "history checkpoint repaired",
				slog.String("session_id", sessionID),
				slog.String("history_checkpoint_id", row.ID.String()),
				slog.String("kept_history_checkpoint_id", held.ID),
				slog.String("reason", v.reason))
			countRepair(ctx, v.reason)
		}
		if v.recorded {
			if _, err := s.queries.ForgetHistoryAppearedRows(ctx, sqlc.ForgetHistoryAppearedRowsParams{
				SessionID:    pgSessionID,
				CheckpointID: dbpkg.ParseUUIDOrEmpty(held.ID),
			}); err != nil {
				return Checkpoint{}, fmt.Errorf("forget history appeared rows: %w", err)
			}
		}
		return held, nil
	}
	return Checkpoint{}, fmt.Errorf("load active history checkpoint: session %s kept moving", sessionID)
}

// KeptBeforeTurn is the checkpoint the session keeps once the turn's context
// rows are replaced, read as Active reads one but without making it active:
// what an edit or retry renders before its replacement commits.
func (s *Store) KeptBeforeTurn(ctx context.Context, sessionID, turnID string) (Checkpoint, error) {
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return Checkpoint{}, err
	}
	pgTurnID, err := dbpkg.ParseUUID(turnID)
	if err != nil {
		return Checkpoint{}, err
	}
	id, err := s.queries.GetHistoryCheckpointKeptBeforeTurn(ctx, sqlc.GetHistoryCheckpointKeptBeforeTurnParams{SessionID: pgSessionID, TurnID: pgTurnID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, fmt.Errorf("load history checkpoint kept before turn: %w", err)
	}
	if !id.Valid {
		return Checkpoint{}, nil
	}
	row, err := s.queries.GetHistoryCheckpoint(ctx, id)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("load history checkpoint kept before turn: %w", err)
	}
	held, _, err := holding(ctx, s.queries, row)
	return held, err
}

// verdict is what a check found of a checkpoint.
type verdict struct {
	// reason is why it does not hold on its own: empty when it does.
	reason string
	// recorded reports that its session has rows on record as appearing
	// below a checkpoint.
	recorded bool
}

// holding is the newest member of the chain from row that holds on its own,
// resolved, and what the check of row itself found. The zero Checkpoint when
// no member holds.
func holding(ctx context.Context, q dbstore.Queries, row sqlc.BotHistoryCheckpoint) (Checkpoint, verdict, error) {
	resolved, v, err := resolve(ctx, q, row)
	if err != nil || v.reason == "" {
		return resolved, v, err
	}
	chain, err := q.ListHistoryCheckpointChain(ctx, row.ID)
	if err != nil {
		return Checkpoint{}, verdict{}, fmt.Errorf("load history checkpoint chain: %w", err)
	}
	for _, member := range chain {
		if member.ID == row.ID {
			continue
		}
		held, memberVerdict, err := resolve(ctx, q, member)
		if err != nil {
			return Checkpoint{}, verdict{}, err
		}
		if memberVerdict.reason == "" {
			return held, v, nil
		}
	}
	return Checkpoint{}, v, nil
}

// resolve reads row as a request renders it, with what its check found.
func resolve(ctx context.Context, q dbstore.Queries, row sqlc.BotHistoryCheckpoint) (Checkpoint, verdict, error) {
	checkpoint, err := fromRow(row)
	if err != nil {
		return Checkpoint{}, verdict{}, err
	}
	v, err := check(ctx, q, row.ID)
	if err != nil || v.reason != "" {
		return Checkpoint{}, v, err
	}
	ids := checkpoint.SummaryIDs()
	if len(ids) == 0 {
		return checkpoint, v, nil
	}
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		pgID, err := dbpkg.ParseUUID(id)
		if err != nil {
			return Checkpoint{}, verdict{}, fmt.Errorf("history checkpoint %s: %w", checkpoint.ID, err)
		}
		pgIDs = append(pgIDs, pgID)
	}
	rows, err := q.ListHistoryCheckpointSummaries(ctx, sqlc.ListHistoryCheckpointSummariesParams{SessionID: row.SessionID, Ids: pgIDs})
	if err != nil {
		return Checkpoint{}, verdict{}, fmt.Errorf("load history checkpoint summaries: %w", err)
	}
	texts := make(map[string]string, len(rows))
	for _, summary := range rows {
		texts[summary.ID.String()] = summary.Summary
	}
	for i, item := range checkpoint.Items {
		if item.SummaryID == "" {
			continue
		}
		text, ok := texts[item.SummaryID]
		if !ok {
			return Checkpoint{}, verdict{reason: reasonMissingSummary, recorded: v.recorded}, nil
		}
		checkpoint.Items[i].Summary = text
	}
	return checkpoint, v, nil
}

// check reports whether the checkpoint holds on its own.
func check(ctx context.Context, q dbstore.Queries, id pgtype.UUID) (verdict, error) {
	result, err := q.CheckHistoryCheckpoint(ctx, id)
	if err != nil {
		return verdict{}, fmt.Errorf("check history checkpoint: %w", err)
	}
	v := verdict{recorded: result.RecordedRows > 0}
	switch {
	case result.MissingRows > 0:
		v.reason = reasonMissingRow
	case result.MissingSummaries > 0:
		v.reason = reasonMissingSummary
	case result.AppearedRows > 0:
		v.reason = reasonRowAppeared
	}
	return v, nil
}

func countRepair(ctx context.Context, reason string) {
	counter, err := otel.Meter(meterName).Int64Counter("history.checkpoint.repairs_total")
	if err != nil {
		return
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func countUnseenRows(ctx context.Context, rows int) {
	counter, err := otel.Meter(meterName).Int64Counter("history.checkpoint.unseen_rows_total")
	if err != nil {
		return
	}
	counter.Add(ctx, int64(rows))
}
