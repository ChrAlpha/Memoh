package agenttest

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// RecordingCompactionLog serves compaction log reads from fixtures and counts the calls.
type RecordingCompactionLog struct {
	dbstore.Queries
	Logs      []sqlc.BotHistoryMessageCompact
	Refs      map[pgtype.UUID][]sqlc.ListMessageRefsByCompactIDRow
	ByID      map[pgtype.UUID]sqlc.BotHistoryMessageCompact
	SessionID pgtype.UUID
	ListCalls int
	RefCalls  []pgtype.UUID
	GetCalls  []pgtype.UUID
	ListErr   error
}

func (q *RecordingCompactionLog) GetCompactionLogByID(_ context.Context, compactID pgtype.UUID) (sqlc.BotHistoryMessageCompact, error) {
	q.GetCalls = append(q.GetCalls, compactID)
	return q.ByID[compactID], nil
}

func (q *RecordingCompactionLog) ListCompactionArtifactParentIDsBySuccessor(_ context.Context, arg sqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error) {
	var ids []pgtype.UUID
	for _, row := range q.ByID {
		if row.Status == "ok" && row.SupersededBy == arg.SuccessorID && row.BotID == arg.BotID && row.SessionID == arg.SessionID {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

func (q *RecordingCompactionLog) ListCompactionArtifactLineageBySession(_ context.Context, sessionID pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	q.SessionID = sessionID
	q.ListCalls++
	return q.Logs, q.ListErr
}

func (q *RecordingCompactionLog) ListMessageRefsByCompactID(_ context.Context, compactID pgtype.UUID) ([]sqlc.ListMessageRefsByCompactIDRow, error) {
	q.RefCalls = append(q.RefCalls, compactID)
	return q.Refs[compactID], nil
}
