package plan

import (
	"context"
	"fmt"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

// ApprovalExecutions marks the decision of an approved call executing, which is
// how a run that resumes after a shutdown knows the call may already have run.
type ApprovalExecutions struct {
	queries dbstore.Queries
}

// NewApprovalExecutions returns the ExecutionMarker over a database.
func NewApprovalExecutions(queries dbstore.Queries) ApprovalExecutions {
	return ApprovalExecutions{queries: queries}
}

// MarkExecuting claims the decision's one execution under the run's fence. A
// context without a fence is a run nobody fences and marks nothing; a claim the
// run does not win means it no longer owns the decision.
func (e ApprovalExecutions) MarkExecuting(ctx context.Context, runID, decisionID string) error {
	fence, ok := runtimefence.FromContext(ctx)
	if !ok {
		return nil
	}
	var count int64
	err := runtimefence.InTransaction(ctx, e.queries, fence.BotID, fence.SessionID, func(queries dbstore.Queries) error {
		var err error
		count, err = queries.MarkSessionRunDecisionExecuting(ctx, sqlc.MarkSessionRunDecisionExecutingParams{
			BotID: db.ParseUUIDOrEmpty(fence.BotID), SessionID: db.ParseUUIDOrEmpty(fence.SessionID),
			RunID: db.ParseUUIDOrEmpty(runID), FencingToken: fence.Token, DecisionID: decisionID,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("checkpoint approved tool execution: %w", err)
	}
	if count != 1 {
		return sessionruntime.ErrRunOwnershipLost
	}
	return nil
}

var _ ExecutionMarker = ApprovalExecutions{}
