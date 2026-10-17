package agenttest

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// RecordingLifecycleStore is a context-lifecycle store double that records every write.
type RecordingLifecycleStore struct {
	Creates             []sqlc.CreateContextLifecycleParams
	CreateErr           error
	Existing            *sqlc.GetContextLifecycleByRunIDRow
	getErr              error
	GetCalls            int
	Updates             []sqlc.UpdateAbortedContextLifecycleSnapshotParams
	updateErr           error
	AssistantID         pgtype.UUID
	Metadata            []byte
	MetadataErr         error
	upserts             []sqlc.UpsertAbortedContextLifecycleParams
	upsertErr           error
	TerminalUpserts     []sqlc.UpsertTerminalContextLifecycleParams
	TerminalUpsertErr   error
	TerminalUpsertBound bool
	TerminalRows        []sqlc.ListTerminalSessionRunsNeedingContextLifecycleRow
	TerminalListErr     error
	TerminalListCalls   int
	TerminalListLimit   int32
	TerminalListBound   bool
	TerminalListWait    bool
}

func (s *RecordingLifecycleStore) CreateContextLifecycle(
	_ context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.CreateContextLifecycleRow, error) {
	s.Creates = append(s.Creates, arg)
	if s.CreateErr != nil {
		return sqlc.CreateContextLifecycleRow{}, s.CreateErr
	}
	created := sqlc.GetContextLifecycleByRunIDRow{
		RunID: arg.RunID, BotID: arg.BotID, SessionID: arg.SessionID,
		Status: arg.Status, ErrorCode: arg.ErrorCode, Snapshot: arg.Snapshot,
	}
	s.Existing = &created
	return sqlc.CreateContextLifecycleRow{
		RunID: arg.RunID, BotID: arg.BotID, SessionID: arg.SessionID,
		Status: arg.Status, ErrorCode: arg.ErrorCode,
	}, nil
}

func (s *RecordingLifecycleStore) GetContextLifecycleByRunID(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.GetContextLifecycleByRunIDRow, error) {
	s.GetCalls++
	if s.getErr != nil {
		return sqlc.GetContextLifecycleByRunIDRow{}, s.getErr
	}
	if s.Existing == nil {
		return sqlc.GetContextLifecycleByRunIDRow{}, pgx.ErrNoRows
	}
	return *s.Existing, nil
}

func (s *RecordingLifecycleStore) GetLatestAssistantContextLifecycleMetadataByRunID(
	_ context.Context,
	_ pgtype.UUID,
) ([]byte, error) {
	return s.Metadata, s.MetadataErr
}

func (s *RecordingLifecycleStore) GetLatestAssistantContextLifecycleByRunID(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.GetLatestAssistantContextLifecycleByRunIDRow, error) {
	if s.MetadataErr != nil {
		return sqlc.GetLatestAssistantContextLifecycleByRunIDRow{}, s.MetadataErr
	}
	if s.Metadata == nil {
		return sqlc.GetLatestAssistantContextLifecycleByRunIDRow{}, pgx.ErrNoRows
	}
	return sqlc.GetLatestAssistantContextLifecycleByRunIDRow{
		ID:       s.AssistantID,
		Metadata: s.Metadata,
	}, nil
}

func (s *RecordingLifecycleStore) UpdateAbortedContextLifecycleSnapshot(
	_ context.Context,
	arg sqlc.UpdateAbortedContextLifecycleSnapshotParams,
) (sqlc.UpdateAbortedContextLifecycleSnapshotRow, error) {
	s.Updates = append(s.Updates, arg)
	return sqlc.UpdateAbortedContextLifecycleSnapshotRow{}, s.updateErr
}

func (s *RecordingLifecycleStore) UpsertAbortedContextLifecycle(
	_ context.Context,
	arg sqlc.UpsertAbortedContextLifecycleParams,
) (sqlc.UpsertAbortedContextLifecycleRow, error) {
	s.upserts = append(s.upserts, arg)
	return sqlc.UpsertAbortedContextLifecycleRow{}, s.upsertErr
}

func (s *RecordingLifecycleStore) UpsertTerminalContextLifecycle(
	ctx context.Context,
	arg sqlc.UpsertTerminalContextLifecycleParams,
) (sqlc.UpsertTerminalContextLifecycleRow, error) {
	_, s.TerminalUpsertBound = ctx.Deadline()
	s.TerminalUpserts = append(s.TerminalUpserts, arg)
	if s.TerminalUpsertErr != nil {
		return sqlc.UpsertTerminalContextLifecycleRow{}, s.TerminalUpsertErr
	}
	upserted := sqlc.GetContextLifecycleByRunIDRow{
		RunID: arg.RunID, BotID: arg.BotID, SessionID: arg.SessionID,
		Status: arg.Status, ErrorCode: arg.ErrorCode, Snapshot: arg.Snapshot,
	}
	s.Existing = &upserted
	return sqlc.UpsertTerminalContextLifecycleRow{
		RunID: arg.RunID, BotID: arg.BotID, SessionID: arg.SessionID,
		Status: arg.Status, ErrorCode: arg.ErrorCode,
	}, nil
}

func (s *RecordingLifecycleStore) ListTerminalSessionRunsNeedingContextLifecycle(
	ctx context.Context,
	batchSize int32,
) ([]sqlc.ListTerminalSessionRunsNeedingContextLifecycleRow, error) {
	s.TerminalListCalls++
	s.TerminalListLimit = batchSize
	_, s.TerminalListBound = ctx.Deadline()
	if s.TerminalListWait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]sqlc.ListTerminalSessionRunsNeedingContextLifecycleRow(nil), s.TerminalRows...), s.TerminalListErr
}
