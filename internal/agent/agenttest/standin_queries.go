package agenttest

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// StandInQueries is the database of a flow test that names none: no bots,
// identities, users or sessions, no decisions, no compaction log, and nothing
// recorded. Any other query panics, which names the test that needs a real
// store.
type StandInQueries struct {
	dbstore.Queries
	bots      NoBotQueries
	store     EmptyStore
	decisions DecisionQueries
	compact   RecordingCompactionLog
}

func (q *StandInQueries) GetBotByID(ctx context.Context, id pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return q.bots.GetBotByID(ctx, id)
}

func (q *StandInQueries) GetChannelIdentityByID(ctx context.Context, id pgtype.UUID) (sqlc.ChannelIdentity, error) {
	return q.bots.GetChannelIdentityByID(ctx, id)
}

func (q *StandInQueries) GetUserByID(ctx context.Context, id pgtype.UUID) (sqlc.User, error) {
	return q.bots.GetUserByID(ctx, id)
}

func (q *StandInQueries) GetSessionByID(ctx context.Context, id pgtype.UUID) (sqlc.BotSession, error) {
	return q.store.GetSessionByID(ctx, id)
}

func (q *StandInQueries) GetTurnAgentTurnID(ctx context.Context, arg sqlc.GetTurnAgentTurnIDParams) (string, error) {
	return q.store.GetTurnAgentTurnID(ctx, arg)
}

func (q *StandInQueries) UpdateSessionModelPreference(ctx context.Context, arg sqlc.UpdateSessionModelPreferenceParams) error {
	return q.store.UpdateSessionModelPreference(ctx, arg)
}

func (q *StandInQueries) DeleteRuntimeDecisionProjectionsByRun(ctx context.Context, arg sqlc.DeleteRuntimeDecisionProjectionsByRunParams) (int64, error) {
	return q.store.DeleteRuntimeDecisionProjectionsByRun(ctx, arg)
}

func (q *StandInQueries) ListToolApprovalsByRun(ctx context.Context, id pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	return q.decisions.ListToolApprovalsByRun(ctx, id)
}

func (q *StandInQueries) ListUserInputsByRun(ctx context.Context, id pgtype.UUID) ([]sqlc.UserInputRequest, error) {
	return q.decisions.ListUserInputsByRun(ctx, id)
}

func (q *StandInQueries) GetToolApprovalRequest(ctx context.Context, id pgtype.UUID) (sqlc.ToolApprovalRequest, error) {
	return q.decisions.GetToolApprovalRequest(ctx, id)
}

func (q *StandInQueries) GetCompactionLogByID(ctx context.Context, id pgtype.UUID) (sqlc.BotHistoryMessageCompact, error) {
	return q.compact.GetCompactionLogByID(ctx, id)
}

func (q *StandInQueries) ListCompactionArtifactParentIDsBySuccessor(ctx context.Context, arg sqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error) {
	return q.compact.ListCompactionArtifactParentIDsBySuccessor(ctx, arg)
}

func (q *StandInQueries) ListCompactionArtifactLineageBySession(ctx context.Context, id pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return q.compact.ListCompactionArtifactLineageBySession(ctx, id)
}

func (q *StandInQueries) ListMessageRefsByCompactID(ctx context.Context, id pgtype.UUID) ([]sqlc.ListMessageRefsByCompactIDRow, error) {
	return q.compact.ListMessageRefsByCompactID(ctx, id)
}

func (*StandInQueries) GetDiscussSession(context.Context, sqlc.GetDiscussSessionParams) (sqlc.GetDiscussSessionRow, error) {
	return sqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}
