package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
)

const (
	contextLifecycleStatusCompleted      = "completed"
	contextLifecycleStatusFailedBudget   = "failed_budget"
	contextLifecycleStatusFailedProvider = "failed_provider"
	contextLifecycleStatusFallback       = "fallback"
	contextLifecycleStatusAborted        = "aborted"
	contextLifecycleWriteTimeout         = 10 * time.Second
)

type contextLifecycleStore interface {
	CreateContextLifecycle(context.Context, sqlc.CreateContextLifecycleParams) (sqlc.ContextLifecycle, error)
	GetContextLifecycleByRunID(context.Context, pgtype.UUID) (sqlc.ContextLifecycle, error)
	GetLatestAssistantContextLifecycleMetadataByRunID(context.Context, pgtype.UUID) ([]byte, error)
	UpdateAbortedContextLifecycleSnapshot(context.Context, sqlc.UpdateAbortedContextLifecycleSnapshotParams) (sqlc.ContextLifecycle, error)
	UpsertAbortedContextLifecycle(context.Context, sqlc.UpsertAbortedContextLifecycleParams) (sqlc.ContextLifecycle, error)
}

func (s *Service) contextLifecycleTerminal(ctx context.Context, cfg native.RunConfig) func(error) {
	var once sync.Once
	return func(cause error) {
		once.Do(func() {
			s.persistRunContextLifecycle(ctx, cfg, cause)
		})
	}
}

func minimalContextLifecycleSnapshot() contextfrag.LifecycleSnapshot {
	return contextfrag.BuildLifecycleSnapshot(contextfrag.BuildManifest(nil))
}

// EnsureTerminalContextLifecycle fills the terminal audit row when an admitted
// run fails before native context assembly has produced a lifecycle holder.
//
// Normal terminal writers run synchronously before the durable run finisher and
// therefore win. The read-before-create keeps this fallback from replacing
// their authoritative snapshot, while a content-light empty manifest gives
// pre-context failures the same run-keyed audit boundary without inventing
// prompt content.
func (s *Service) EnsureTerminalContextLifecycle(
	ctx context.Context,
	runID, botID, sessionID string,
	cause error,
) {
	if s == nil || s.contextLifecycles == nil || runOwnershipLost(ctx) ||
		errors.Is(cause, sessionruntime.ErrRunOwnershipLost) {
		return
	}
	snapshot := minimalContextLifecycleSnapshot()
	status, _ := classifyContextLifecycleTerminal(snapshot, cause)
	runUUID, botUUID, sessionUUID, err := parseContextLifecycleIDs(runID, botID, sessionID)
	if err != nil {
		s.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contextLifecycleWriteTimeout)
	defer cancel()
	existing, err := s.contextLifecycles.GetContextLifecycleByRunID(readCtx, runUUID)
	if err == nil {
		if existing.BotID != botUUID || existing.SessionID != sessionUUID {
			s.recordContextLifecyclePersistenceError(
				errors.New("existing context lifecycle identity does not match terminal fallback"),
				runID,
				botID,
				sessionID,
				status,
			)
		}
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	s.persistContextLifecycleSnapshot(
		ctx,
		runID,
		botID,
		sessionID,
		&snapshot,
		cause,
		false,
	)
}

func (s *Service) persistRunContextLifecycle(ctx context.Context, cfg native.RunConfig, cause error) {
	if cfg.ContextLifecycle == nil {
		return
	}
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		return
	}
	s.persistContextLifecycleSnapshot(
		ctx,
		cfg.RunID,
		cfg.Identity.BotID,
		cfg.Identity.SessionID,
		&snapshot,
		cause,
		true,
	)
}

func (s *Service) recoverContextLifecycleFromAssistantMetadata(
	ctx context.Context,
	runID, botID, sessionID string,
	cause error,
) {
	if s == nil || s.contextLifecycles == nil || runOwnershipLost(ctx) ||
		errors.Is(cause, sessionruntime.ErrRunOwnershipLost) {
		return
	}
	runUUID, err := db.ParseUUID(runID)
	if err != nil {
		s.recordContextLifecyclePersistenceError(
			err,
			runID,
			botID,
			sessionID,
			contextLifecycleStatusFailedProvider,
		)
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contextLifecycleWriteTimeout)
	defer cancel()
	if _, err = s.contextLifecycles.GetContextLifecycleByRunID(readCtx, runUUID); err == nil {
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.recordContextLifecyclePersistenceError(
			err,
			runID,
			botID,
			sessionID,
			contextLifecycleStatusFailedProvider,
		)
		return
	}
	metadata, err := s.contextLifecycles.GetLatestAssistantContextLifecycleMetadataByRunID(readCtx, runUUID)
	if err != nil {
		s.recordContextLifecyclePersistenceError(
			err,
			runID,
			botID,
			sessionID,
			contextLifecycleStatusFailedProvider,
		)
		return
	}
	snapshot, ok := contextfrag.LifecycleSnapshotFromMetadata(metadata)
	if !ok {
		s.recordContextLifecyclePersistenceError(
			errors.New("assistant message has no context lifecycle snapshot"),
			runID,
			botID,
			sessionID,
			contextLifecycleStatusFailedProvider,
		)
		return
	}
	s.persistContextLifecycleSnapshot(ctx, runID, botID, sessionID, &snapshot, cause, false)
}

func (s *Service) persistContextLifecycleSnapshot(
	ctx context.Context,
	runID, botID, sessionID string,
	snapshot *contextfrag.LifecycleSnapshot,
	cause error,
	authoritative bool,
) {
	if s == nil || s.contextLifecycles == nil || snapshot == nil || runOwnershipLost(ctx) ||
		errors.Is(cause, sessionruntime.ErrRunOwnershipLost) {
		return
	}
	status, errorCode := classifyContextLifecycleTerminal(*snapshot, cause)
	runUUID, botUUID, sessionUUID, err := parseContextLifecycleIDs(runID, botID, sessionID)
	if err != nil {
		s.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		s.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	var code pgtype.Text
	if errorCode != "" {
		code = pgtype.Text{String: errorCode, Valid: true}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contextLifecycleWriteTimeout)
	defer cancel()
	_, err = s.contextLifecycles.CreateContextLifecycle(writeCtx, sqlc.CreateContextLifecycleParams{
		RunID:     runUUID,
		BotID:     botUUID,
		SessionID: sessionUUID,
		Status:    status,
		ErrorCode: code,
		Snapshot:  raw,
	})
	if err == nil {
		return
	}
	if !db.IsUniqueViolation(err) {
		s.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	if !authoritative {
		return
	}
	_, err = s.contextLifecycles.UpdateAbortedContextLifecycleSnapshot(
		writeCtx,
		sqlc.UpdateAbortedContextLifecycleSnapshotParams{
			Snapshot:  raw,
			RunID:     runUUID,
			BotID:     botUUID,
			SessionID: sessionUUID,
		},
	)
	if err == nil {
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := s.contextLifecycles.GetContextLifecycleByRunID(writeCtx, runUUID)
		if getErr == nil && existing.BotID == botUUID && existing.SessionID == sessionUUID {
			return
		}
		if getErr != nil {
			err = getErr
		} else {
			err = errors.New("existing context lifecycle identity does not match terminal write")
		}
	}
	s.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
}

func classifyContextLifecycleTerminal(snapshot contextfrag.LifecycleSnapshot, cause error) (string, string) {
	var budgetFailure, fallback bool
	var budgetReason string
	for _, mutation := range snapshot.Mutations {
		switch mutation.Kind {
		case contextfrag.MutationContextBudgetFailure:
			budgetFailure = true
			budgetReason = strings.TrimSpace(mutation.Detail)
		case contextfrag.MutationContextViewFallback:
			fallback = true
		}
	}
	if budgetFailure || errors.Is(cause, contextfrag.ErrProtectedContextOverflow) || errors.Is(cause, contextfrag.ErrBudgetUnsatisfied) {
		code := apperror.CodeOf(cause)
		switch {
		case code == apperror.CodeContextProtectedOverflow, code == apperror.CodeContextBudgetUnsatisfied:
			return contextLifecycleStatusFailedBudget, string(code)
		case errors.Is(cause, contextfrag.ErrProtectedContextOverflow), budgetReason == "protected_context_overflow":
			return contextLifecycleStatusFailedBudget, string(apperror.CodeContextProtectedOverflow)
		default:
			return contextLifecycleStatusFailedBudget, string(apperror.CodeContextBudgetUnsatisfied)
		}
	}
	if errors.Is(cause, context.Canceled) {
		return contextLifecycleStatusAborted, ""
	}
	if cause != nil {
		return contextLifecycleStatusFailedProvider, string(apperror.CodeOf(cause))
	}
	if fallback {
		return contextLifecycleStatusFallback, ""
	}
	return contextLifecycleStatusCompleted, ""
}

func parseContextLifecycleIDs(runID, botID, sessionID string) (pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
	runUUID, err := db.ParseUUID(runID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	botUUID, err := db.ParseUUID(botID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	sessionUUID, err := db.ParseUUID(sessionID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return runUUID, botUUID, sessionUUID, nil
}

func (s *Service) recordContextLifecyclePersistenceError(
	err error,
	runID, botID, sessionID, status string,
) {
	count := s.contextLifecyclePersistenceErrors.Add(1)
	if s.logger == nil {
		return
	}
	s.logger.Error("persist context lifecycle failed",
		slog.Any("error", err),
		slog.String("run_id", runID),
		slog.String("bot_id", botID),
		slog.String("session_id", sessionID),
		slog.String("status", status),
		slog.Uint64("failure_count", count),
	)
}
