package thread

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

var ErrExternalChildCapacity = errors.New("external child execution capacity is held")

type externalExecutionQueries interface {
	LockBotForSessionWrite(context.Context, pgtype.UUID) (pgtype.UUID, error)
	ListExternalSubagentExecutionHolds(context.Context, pgtype.UUID) ([]sqlc.BotSession, error)
	BeginExternalSubagentExecution(context.Context, sqlc.BeginExternalSubagentExecutionParams) (sqlc.BotSession, error)
	FinishExternalSubagentExecution(context.Context, sqlc.FinishExternalSubagentExecutionParams) (int64, error)
	ClaimExternalSubagentReap(context.Context, sqlc.ClaimExternalSubagentReapParams) (sqlc.BotSession, error)
	ResolveExternalSubagentTermination(context.Context, sqlc.ResolveExternalSubagentTerminationParams) (sqlc.BotSession, error)
}

func (s *Service) externalExecutionQueries() (externalExecutionQueries, error) {
	q, ok := s.queries.(externalExecutionQueries)
	if !ok {
		return nil, errors.New("external child execution store is unavailable")
	}
	return q, nil
}

// BeginExternalSubagentExecution reserves capacity under the bot lock. The
// count happens after acquiring the lock, in a separate statement, so two
// servers cannot both reserve the last slot from an old statement snapshot.
func (s *Service) BeginExternalSubagentExecution(ctx context.Context, child Thread, runID string, limit int) error {
	fence, ok := runtimefence.FromContext(ctx)
	if !ok || fence.BotID != child.BotID || fence.SessionID != child.ID || fence.Token <= 0 {
		return ErrInvalidExternalSubagent
	}
	tx, ok := s.queries.(subagentTransactionalQueries)
	if !ok {
		return errors.New("external child execution requires transactional storage")
	}
	return tx.InTx(ctx, func(raw dbstore.Queries) error {
		q, ok := raw.(externalExecutionQueries)
		if !ok {
			return errors.New("external child execution queries unavailable")
		}
		botID, err := dbpkg.ParseUUID(child.BotID)
		if err != nil {
			return err
		}
		if _, err = q.LockBotForSessionWrite(ctx, botID); err != nil {
			return err
		}
		helds, err := q.ListExternalSubagentExecutionHolds(ctx, botID)
		if err != nil {
			return err
		}
		if limit <= 0 || len(helds) >= limit {
			return ErrExternalChildCapacity
		}
		sessionID, err := dbpkg.ParseUUID(child.ID)
		if err != nil {
			return err
		}
		_, err = q.BeginExternalSubagentExecution(ctx, sqlc.BeginExternalSubagentExecutionParams{
			BotID: botID, SessionID: sessionID, RuntimeType: child.RuntimeType, RunID: runID, FencingToken: fence.Token,
		})
		return err
	})
}

func (s *Service) FinishExternalSubagentExecution(ctx context.Context, sessionID, runID string) error {
	q, err := s.externalExecutionQueries()
	if err != nil {
		return err
	}
	id, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return err
	}
	fence, ok := runtimefence.FromContext(ctx)
	if !ok || fence.SessionID != sessionID || fence.Token <= 0 {
		return ErrInvalidExternalSubagent
	}
	n, err := q.FinishExternalSubagentExecution(ctx, sqlc.FinishExternalSubagentExecutionParams{SessionID: id, RunID: runID, FencingToken: fence.Token})
	if err == nil && n != 1 {
		return ErrRuntimeMetadataStale
	}
	return err
}

func (s *Service) ListExternalExecutionHolds(ctx context.Context, botID string) ([]Thread, error) {
	q, err := s.externalExecutionQueries()
	if err != nil {
		return nil, err
	}
	id, err := dbpkg.ParseUUID(botID)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListExternalSubagentExecutionHolds(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(rows))
	for _, row := range rows {
		out = append(out, toThread(row))
	}
	return out, nil
}

func (s *Service) ClaimExternalSubagentReap(ctx context.Context, sessionID, runID, claim string) (bool, error) {
	q, err := s.externalExecutionQueries()
	if err != nil {
		return false, err
	}
	id, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return false, err
	}
	_, err = q.ClaimExternalSubagentReap(ctx, sqlc.ClaimExternalSubagentReapParams{SessionID: id, RunID: runID, Claim: claim})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) ResolveExternalSubagentTermination(ctx context.Context, sessionID, runID, claim string) (bool, error) {
	q, err := s.externalExecutionQueries()
	if err != nil {
		return false, err
	}
	id, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return false, err
	}
	_, err = q.ResolveExternalSubagentTermination(ctx, sqlc.ResolveExternalSubagentTerminationParams{SessionID: id, RunID: runID, Claim: claim})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
