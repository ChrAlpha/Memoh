package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// Copy is the history a fork copied from its source session.
type Copy struct {
	SourceSessionID string
	ForkSessionID   string
	// SourceMessageIDs and ForkMessageIDs pair each copied source row with
	// its copy.
	SourceMessageIDs []pgtype.UUID
	ForkMessageIDs   []pgtype.UUID
	// ThroughTurnPosition is the source position of the last turn the fork
	// copied.
	ThroughTurnPosition int64
}

// CopyToFork gives a fork the newest checkpoint on its source's active chain
// that holds and accounts for no history past the copy, in the transaction q
// runs in: its summaries become the fork's own, the rows it keeps are named
// by their copies, its position becomes the copy's place for it, and what it
// omits stays omitted, as the copied rows keep their creation times. The
// fork's checkpoint has no parent: nothing before it exists in the fork. It
// returns the zero Checkpoint when no checkpoint of the source holds for the
// copy.
func CopyToFork(ctx context.Context, q dbstore.Queries, c Copy) (Checkpoint, error) {
	sourceID, err := dbpkg.ParseUUID(c.SourceSessionID)
	if err != nil {
		return Checkpoint{}, err
	}
	forkID, err := dbpkg.ParseUUID(c.ForkSessionID)
	if err != nil {
		return Checkpoint{}, err
	}
	active, err := q.GetActiveHistoryCheckpoint(ctx, sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, nil
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("load source history checkpoint: %w", err)
	}
	chain, err := q.ListHistoryCheckpointChain(ctx, active.ID)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("load source history checkpoint chain: %w", err)
	}
	copies := make(map[string]pgtype.UUID, len(c.SourceMessageIDs))
	for i, id := range c.SourceMessageIDs {
		if i < len(c.ForkMessageIDs) {
			copies[id.String()] = c.ForkMessageIDs[i]
		}
	}
	for _, member := range chain {
		if member.TurnPosition > c.ThroughTurnPosition {
			continue
		}
		source, v, err := resolve(ctx, q, member)
		if err != nil {
			return Checkpoint{}, err
		}
		if v.reason != "" || !copiedRows(source, copies) {
			continue
		}
		items := make([]Item, 0, len(source.Items))
		for _, item := range source.Items {
			if item.RowID != "" {
				items = append(items, Item{RowID: copies[item.RowID].String()})
				continue
			}
			summaryID, err := dbpkg.ParseUUID(item.SummaryID)
			if err != nil {
				return Checkpoint{}, err
			}
			copied, err := q.CopyCompactionSummary(ctx, sqlc.CopyCompactionSummaryParams{SessionID: forkID, SourceCompactID: summaryID})
			if err != nil {
				return Checkpoint{}, fmt.Errorf("copy summary %s: %w", item.SummaryID, err)
			}
			items = append(items, Item{SummaryID: copied.String(), Summary: item.Summary})
		}
		position, err := q.GetForkHistoryPosition(ctx, sqlc.GetForkHistoryPositionParams{
			TurnPosition:     source.Position.Turn,
			TurnMessageSeq:   source.Position.Seq,
			SourceMessageIds: c.SourceMessageIDs,
			ForkMessageIds:   c.ForkMessageIDs,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Checkpoint{}, fmt.Errorf("map history checkpoint position: %w", err)
		}
		return create(ctx, q, forkID, Checkpoint{}, messagepkg.HistoryPosition{Turn: position.TurnPosition, Seq: position.TurnMessageSeq}, source.Omitted, items)
	}
	return Checkpoint{}, nil
}

// copiedRows reports whether the fork copied every row the checkpoint keeps.
func copiedRows(checkpoint Checkpoint, copies map[string]pgtype.UUID) bool {
	for _, row := range checkpoint.RowIDs() {
		if _, ok := copies[row]; !ok {
			return false
		}
	}
	return true
}
