package compaction

import (
	"context"
	"fmt"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	"github.com/felinics/memoh/internal/chat/thread"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// ForkHistoryCarrier gives a fork the history checkpoint that holds for its
// copy (thread.ForkHistoryCarrier).
type ForkHistoryCarrier struct{}

// CarryForkHistory makes the newest checkpoint of the source that holds for
// the fork's copy the fork's own, so the fork's first request holds what the
// source's requests held at the fork point without compacting it again.
func (ForkHistoryCarrier) CarryForkHistory(ctx context.Context, queries dbstore.Queries, fork thread.ForkedHistory) error {
	if _, err := checkpoint.CopyToFork(ctx, queries, checkpoint.Copy{
		SourceSessionID:     fork.SourceSessionID,
		ForkSessionID:       fork.ForkSessionID,
		SourceMessageIDs:    fork.SourceMessageIDs,
		ForkMessageIDs:      fork.ForkMessageIDs,
		ThroughTurnPosition: fork.ThroughTurnPosition,
	}); err != nil {
		return fmt.Errorf("copy history checkpoint: %w", err)
	}
	return nil
}
