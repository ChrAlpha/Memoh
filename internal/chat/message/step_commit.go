package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

var ErrAgentStepNotWritable = errors.New("agent step is no longer writable")

type agentStepQueries interface {
	MergeTurnRequestRowMetadata(context.Context, sqlc.MergeTurnRequestRowMetadataParams) (int64, error)
	MergeRunInterruptedRowMetadata(context.Context, sqlc.MergeRunInterruptedRowMetadataParams) (int64, error)
	LockSessionRunForAgentStepCommit(context.Context, sqlc.LockSessionRunForAgentStepCommitParams) (pgtype.UUID, error)
}

// PersistAgentStep appends one SDK step in a runtime-fenced transaction.
// Complete steps precede abort intent; interrupted checkpoints remain writable
// until terminal finalization for cancellation paths without recorded intent.
func (s *DBService) PersistAgentStep(ctx context.Context, step AgentStep) ([]Message, error) {
	botID, sessionID, err := validateAgentStep(ctx, s, step)
	if err != nil {
		return nil, err
	}
	if !step.Interrupted {
		step.Messages = append([]PersistInput(nil), step.Messages...)
		for i := range step.Messages {
			step.Messages[i] = s.prepareMarkdownMedia(ctx, step.Messages[i])
		}
	}
	var persisted []Message
	err = runtimefence.InTransaction(ctx, s.queries, botID, sessionID, func(queries dbstore.Queries) error {
		var txErr error
		persisted, txErr = s.persistAgentStepTx(ctx, queries, step)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	for _, message := range persisted {
		s.publishMessageCreated(message)
	}
	return persisted, nil
}

// RecordInterruptedStepFailure writes the failure a run was named with onto the
// interrupted rows the run wrote. It takes the same lock as an interrupted
// step, so a revoked fence or a finished run is ErrAgentStepNotWritable.
func (s *DBService) RecordInterruptedStepFailure(ctx context.Context, failure InterruptedStepFailure) error {
	if s == nil || s.queries == nil {
		return errors.New("message service is not configured")
	}
	runID, botID, sessionID := strings.TrimSpace(failure.RunID), strings.TrimSpace(failure.BotID), strings.TrimSpace(failure.SessionID)
	if runID == "" || botID == "" || sessionID == "" || len(failure.MessageIDs) == 0 || len(failure.Metadata) == 0 {
		return errors.New("interrupted step failure requires run, bot, session, message ids and metadata")
	}
	fence, ok := runtimefence.FromContext(ctx)
	if !ok {
		return errors.New("interrupted step failure requires a runtime persistence fence")
	}
	payload, err := json.Marshal(failure.Metadata)
	if err != nil {
		return fmt.Errorf("encode interrupted step failure: %w", err)
	}
	pgRunID, err := dbpkg.ParseUUID(runID)
	if err != nil {
		return fmt.Errorf("invalid run id: %w", err)
	}
	pgBotID, err := dbpkg.ParseUUID(botID)
	if err != nil {
		return fmt.Errorf("invalid bot id: %w", err)
	}
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return fmt.Errorf("invalid session id: %w", err)
	}
	pgMessageIDs := make([]pgtype.UUID, len(failure.MessageIDs))
	for i, id := range failure.MessageIDs {
		if pgMessageIDs[i], err = dbpkg.ParseUUID(strings.TrimSpace(id)); err != nil {
			return fmt.Errorf("invalid message id: %w", err)
		}
	}
	return runtimefence.InTransaction(ctx, s.queries, botID, sessionID, func(queries dbstore.Queries) error {
		writer, ok := queries.(agentStepQueries)
		if !ok {
			return errors.New("persistence store does not support agent step writes")
		}
		_, lockErr := writer.LockSessionRunForAgentStepCommit(ctx, sqlc.LockSessionRunForAgentStepCommitParams{
			RunID: pgRunID, BotID: pgBotID, SessionID: pgSessionID,
			FencingToken: fence.Token, Interrupted: true,
		})
		if errors.Is(lockErr, pgx.ErrNoRows) {
			return ErrAgentStepNotWritable
		} else if lockErr != nil {
			return fmt.Errorf("lock session run for agent step: %w", lockErr)
		}
		_, err := writer.MergeRunInterruptedRowMetadata(ctx, sqlc.MergeRunInterruptedRowMetadataParams{
			FailureMetadata: payload, BotID: pgBotID, SessionID: pgSessionID, RunID: pgRunID, MessageIds: pgMessageIDs,
		})
		return err
	})
}

func (s *DBService) persistAgentStepTx(ctx context.Context, queries dbstore.Queries, step AgentStep) ([]Message, error) {
	if _, _, err := validateAgentStep(ctx, s, step); err != nil {
		return nil, err
	}
	if queries == nil {
		return nil, errors.New("persistence transaction is not configured")
	}
	runID := strings.TrimSpace(step.RunID)
	botID := strings.TrimSpace(step.Messages[0].BotID)
	sessionID := strings.TrimSpace(step.Messages[0].SessionID)
	fence, _ := runtimefence.FromContext(ctx)
	pgRunID, err := dbpkg.ParseUUID(runID)
	if err != nil {
		return nil, fmt.Errorf("invalid agent step run id: %w", err)
	}
	pgBotID, err := dbpkg.ParseUUID(botID)
	if err != nil {
		return nil, fmt.Errorf("invalid agent step bot id: %w", err)
	}
	pgSessionID, err := dbpkg.ParseUUID(sessionID)
	if err != nil {
		return nil, fmt.Errorf("invalid agent step session id: %w", err)
	}
	writer, ok := queries.(agentStepQueries)
	if !ok {
		return nil, errors.New("persistence store does not support agent step writes")
	}
	params := sqlc.LockSessionRunForAgentStepCommitParams{
		RunID: pgRunID, BotID: pgBotID, SessionID: pgSessionID,
		FencingToken: fence.Token, Interrupted: step.Interrupted,
	}
	_, lockErr := writer.LockSessionRunForAgentStepCommit(ctx, params)
	if errors.Is(lockErr, pgx.ErrNoRows) {
		return nil, ErrAgentStepNotWritable
	} else if lockErr != nil {
		return nil, fmt.Errorf("lock session run for agent step: %w", lockErr)
	}

	txService := *s
	txService.queries = queries
	txService.publisher = nil
	persisted, requestID, err := txService.persistBoundMessages(ctx, step.Messages, step.Replacement)
	if err != nil || len(step.RequestMetadata) == 0 {
		return persisted, err
	}
	if requestID == "" {
		return nil, errors.New("agent step request metadata requires a request row")
	}
	pgRequestID, err := dbpkg.ParseUUID(requestID)
	if err != nil {
		return nil, fmt.Errorf("invalid agent step request id: %w", err)
	}
	payload, err := json.Marshal(step.RequestMetadata)
	if err != nil {
		return nil, fmt.Errorf("encode agent step request metadata: %w", err)
	}
	merged, err := writer.MergeTurnRequestRowMetadata(ctx, sqlc.MergeTurnRequestRowMetadataParams{
		RequestMetadata: payload, BotID: pgBotID, SessionID: pgSessionID, RequestMessageID: pgRequestID,
	})
	if err != nil {
		return nil, fmt.Errorf("record agent step request metadata: %w", err)
	}
	if merged != 1 {
		return nil, fmt.Errorf("agent step request %s is not a request row of the session", requestID)
	}
	return persisted, nil
}

func validateAgentStep(ctx context.Context, s *DBService, step AgentStep) (string, string, error) {
	if s == nil || s.queries == nil {
		return "", "", errors.New("message service is not configured")
	}
	if len(step.Messages) == 0 {
		return "", "", errors.New("agent step requires messages")
	}
	runID := strings.TrimSpace(step.RunID)
	botID := strings.TrimSpace(step.Messages[0].BotID)
	sessionID := strings.TrimSpace(step.Messages[0].SessionID)
	if runID == "" || botID == "" || sessionID == "" {
		return "", "", errors.New("agent step requires run, bot, and session ids")
	}
	for _, input := range step.Messages {
		if input.SkipHistoryTurn || strings.TrimSpace(input.RunID) != runID ||
			strings.TrimSpace(input.BotID) != botID || strings.TrimSpace(input.SessionID) != sessionID {
			return "", "", errors.New("agent step messages must be turn-bound and share one run and session")
		}
	}
	if _, ok := runtimefence.FromContext(ctx); !ok {
		return "", "", errors.New("agent step requires a runtime persistence fence")
	}
	return botID, sessionID, nil
}
