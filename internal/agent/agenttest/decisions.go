package agenttest

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// DecisionQueries serves one user input row and one tool approval row of a run,
// as the reads that finalize a run's decisions see them.
type DecisionQueries struct {
	dbstore.Queries
	Row      sqlc.UserInputRequest
	Approval sqlc.ToolApprovalRequest
}

func (q *DecisionQueries) ListToolApprovalsByRun(_ context.Context, runID pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	if q.Approval.RunID == runID {
		return []sqlc.ToolApprovalRequest{q.Approval}, nil
	}
	return nil, nil
}

func (q *DecisionQueries) ListUserInputsByRun(_ context.Context, runID pgtype.UUID) ([]sqlc.UserInputRequest, error) {
	if q.Row.RunID == runID {
		return []sqlc.UserInputRequest{q.Row}, nil
	}
	return nil, nil
}

func (q *DecisionQueries) GetToolApprovalRequest(context.Context, pgtype.UUID) (sqlc.ToolApprovalRequest, error) {
	return q.Approval, nil
}

// GetDiscussSession reports no discuss session: a service built on these
// queries asks for one when a run ends.
func (*DecisionQueries) GetDiscussSession(context.Context, sqlc.GetDiscussSessionParams) (sqlc.GetDiscussSessionRow, error) {
	return sqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}
