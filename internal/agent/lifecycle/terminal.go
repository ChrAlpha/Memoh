package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/runtimefence"
)

// Terminal statuses of a context lifecycle row.
const (
	StatusCompleted      = "completed"
	StatusFailedBudget   = "failed_budget"
	StatusFailedProvider = "failed_provider"
	StatusFallback       = "fallback"
	StatusAborted        = "aborted"
)

const (
	// WriteTimeout bounds each lifecycle write, which runs detached from the
	// run's cancellation.
	WriteTimeout                                  = 10 * time.Second
	contextLifecycleReconciliationBatchSize int32 = 100
)

// Store is the part of the database the lifecycle table needs.
type Store interface {
	CreateContextLifecycle(context.Context, sqlc.CreateContextLifecycleParams) (sqlc.CreateContextLifecycleRow, error)
	GetContextLifecycleByRunID(context.Context, pgtype.UUID) (sqlc.GetContextLifecycleByRunIDRow, error)
	GetLatestAssistantContextLifecycleMetadataByRunID(context.Context, pgtype.UUID) ([]byte, error)
	UpdateAbortedContextLifecycleSnapshot(context.Context, sqlc.UpdateAbortedContextLifecycleSnapshotParams) (sqlc.UpdateAbortedContextLifecycleSnapshotRow, error)
	UpsertAbortedContextLifecycle(context.Context, sqlc.UpsertAbortedContextLifecycleParams) (sqlc.UpsertAbortedContextLifecycleRow, error)
	UpsertTerminalContextLifecycle(context.Context, sqlc.UpsertTerminalContextLifecycleParams) (sqlc.UpsertTerminalContextLifecycleRow, error)
	ListTerminalSessionRunsNeedingContextLifecycle(context.Context, int32) ([]sqlc.ListTerminalSessionRunsNeedingContextLifecycleRow, error)
}

type lifecycleCandidateKey struct {
	runID        string
	fencingToken int64
}

// CandidateQuality ranks how trustworthy a staged snapshot is; a better one
// replaces a worse one for the same run.
type CandidateQuality uint8

const (
	CandidateMinimal CandidateQuality = iota
	CandidateMetadata
	CandidateAuthoritative
)

type lifecycleCandidate struct {
	botID     string
	sessionID string
	snapshot  []byte
	status    string
	errorCode string
	quality   CandidateQuality
}

// Terminal returns the callback that writes the run's lifecycle row when the
// run ends. Only the first call writes.
func (r *Recorder) Terminal(ctx context.Context, cfg native.RunConfig) func(error) {
	var once sync.Once
	return func(cause error) {
		once.Do(func() {
			r.persistRunContextLifecycle(ctx, cfg, cause)
		})
	}
}

// MinimalSnapshot is the content-light snapshot of a run that never assembled
// a context.
func MinimalSnapshot() contextfrag.LifecycleSnapshot {
	return contextfrag.BuildLifecycleSnapshot(contextfrag.BuildManifest(nil))
}

// StageCandidate keeps an admitted owner's content-light
// snapshot out of the diagnostic table until session_runs has accepted the
// same fencing token's terminal transition. Unfenced callers are direct runs
// with no durable ledger row and keep the immediate-write path below.
func (r *Recorder) StageCandidate(
	ctx context.Context,
	runID, botID, sessionID string,
	snapshot *contextfrag.LifecycleSnapshot,
	cause error,
	quality CandidateQuality,
) bool {
	if r.store == nil {
		return false
	}
	fence, fenced := runtimefence.FromContext(runreq.NonNilContext(ctx))
	if !fenced {
		return false
	}
	status, _ := classifyContextLifecycleTerminal(ctx, contextfrag.LifecycleSnapshot{}, cause)
	if snapshot == nil {
		r.recordContextLifecyclePersistenceError(
			errors.New("context lifecycle candidate snapshot is missing"),
			runID,
			botID,
			sessionID,
			status,
		)
		return true
	}
	status, errorCode := classifyContextLifecycleTerminal(ctx, *snapshot, cause)
	if err := runtimefence.ValidateScope(ctx, botID, sessionID); err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return true
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		r.recordContextLifecyclePersistenceError(
			errors.New("context lifecycle candidate run id is missing"),
			runID,
			botID,
			sessionID,
			status,
		)
		return true
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return true
	}
	candidate := lifecycleCandidate{
		botID:     strings.TrimSpace(botID),
		sessionID: strings.TrimSpace(sessionID),
		snapshot:  append([]byte(nil), raw...),
		status:    status,
		errorCode: errorCode,
		quality:   quality,
	}
	key := lifecycleCandidateKey{runID: runID, fencingToken: fence.Token}
	r.candidatesMu.Lock()
	if r.candidates == nil {
		r.candidates = make(map[lifecycleCandidateKey]lifecycleCandidate)
	}
	existing, exists := r.candidates[key]
	if !exists || candidate.quality >= existing.quality {
		if candidate.errorCode == "" && exists && candidate.status == existing.status {
			candidate.errorCode = existing.errorCode
		}
		r.candidates[key] = candidate
	} else if existing.status == candidate.status && existing.errorCode == "" && candidate.errorCode != "" {
		existing.errorCode = candidate.errorCode
		r.candidates[key] = existing
	}
	r.candidatesMu.Unlock()
	return true
}

// EnsureTerminal records a content-light fallback for runs
// that fail before native context assembly creates a snapshot. A terminal
// writer with an authoritative holder always wins this read-before-create race.
func (r *Recorder) EnsureTerminal(
	ctx context.Context,
	runID, botID, sessionID string,
	cause error,
) {
	if r.store == nil || contextLifecycleOwnershipLost(ctx, cause) {
		return
	}
	ctx = runreq.NonNilContext(ctx)
	snapshot := MinimalSnapshot()
	if r.StageCandidate(
		ctx,
		runID,
		botID,
		sessionID,
		&snapshot,
		cause,
		CandidateMinimal,
	) {
		return
	}
	status, _ := classifyContextLifecycleTerminal(ctx, snapshot, cause)
	runUUID, botUUID, sessionUUID, err := parseIDs(runID, botID, sessionID)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
	defer cancel()
	existing, err := r.store.GetContextLifecycleByRunID(readCtx, runUUID)
	if err == nil {
		if existing.BotID != botUUID || existing.SessionID != sessionUUID {
			r.recordContextLifecyclePersistenceError(
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
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	r.PersistSnapshot(ctx, runID, botID, sessionID, &snapshot, cause, false)
}

func (r *Recorder) persistRunContextLifecycle(ctx context.Context, cfg native.RunConfig, cause error) {
	if cfg.ContextLifecycle == nil {
		return
	}
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		return
	}
	r.PersistSnapshot(
		ctx,
		cfg.RunID,
		cfg.Identity.BotID,
		cfg.Identity.SessionID,
		&snapshot,
		cause,
		true,
	)
}

// recoverFromAssistantMetadata rebuilds a missing row from the snapshot the
// assistant message carries.
func (r *Recorder) recoverFromAssistantMetadata(
	ctx context.Context,
	runID, botID, sessionID string,
	cause error,
) {
	if r.store == nil || contextLifecycleOwnershipLost(ctx, cause) {
		return
	}
	ctx = runreq.NonNilContext(ctx)
	status, _ := classifyContextLifecycleTerminal(ctx, MinimalSnapshot(), cause)
	runUUID, _, _, err := parseIDs(runID, botID, sessionID)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
	defer cancel()
	if _, err = r.store.GetContextLifecycleByRunID(readCtx, runUUID); err == nil {
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	raw, ready, err := r.assistantContextLifecycleSnapshot(readCtx, runUUID)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	if !ready {
		return
	}
	snapshot, err := contextfrag.DecodeLifecycleSnapshot(raw)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	r.PersistSnapshot(ctx, runID, botID, sessionID, &snapshot, cause, false)
}

// PersistSnapshot writes snapshot as the lifecycle row of a finished run, or
// stages it when the run holds a fencing token. Only an authoritative snapshot
// replaces a row an earlier fallback wrote.
func (r *Recorder) PersistSnapshot(
	ctx context.Context,
	runID, botID, sessionID string,
	snapshot *contextfrag.LifecycleSnapshot,
	cause error,
	authoritative bool,
) {
	if r.store == nil || snapshot == nil || contextLifecycleOwnershipLost(ctx, cause) {
		return
	}
	ctx = runreq.NonNilContext(ctx)
	if status, _ := classifyContextLifecycleTerminal(ctx, *snapshot, cause); status == StatusFailedBudget &&
		explicitRunCancellation(ctx, cause) {
		// Budget evidence outranks the abort for the terminal status; keep the
		// concurrent cancellation recoverable from the snapshot itself.
		snapshot.Mutations = append(snapshot.Mutations, contextfrag.MutationRecord{
			Kind:   contextfrag.MutationRunAbortObserved,
			Detail: "explicit_cancel_during_budget_failure",
		})
	}
	quality := CandidateMetadata
	if authoritative {
		quality = CandidateAuthoritative
	}
	if r.StageCandidate(ctx, runID, botID, sessionID, snapshot, cause, quality) {
		return
	}
	status, errorCode := classifyContextLifecycleTerminal(ctx, *snapshot, cause)
	runUUID, botUUID, sessionUUID, err := parseIDs(runID, botID, sessionID)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	var code pgtype.Text
	if errorCode != "" {
		code = pgtype.Text{String: errorCode, Valid: true}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteTimeout)
	defer cancel()
	_, err = r.store.CreateContextLifecycle(writeCtx, sqlc.CreateContextLifecycleParams{
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
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
		return
	}
	if !authoritative {
		return
	}
	_, err = r.store.UpdateAbortedContextLifecycleSnapshot(
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
		existing, getErr := r.store.GetContextLifecycleByRunID(writeCtx, runUUID)
		if getErr == nil && existing.BotID == botUUID && existing.SessionID == sessionUUID {
			return
		}
		if getErr != nil {
			err = getErr
		} else {
			err = errors.New("existing context lifecycle identity does not match terminal write")
		}
	}
	r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, status)
}

// ReconcileTerminal settles the lifecycle row of a run whose terminal
// transition was accepted, choosing between the staged candidate, the existing
// row and what the assistant message recorded.
func (r *Recorder) ReconcileTerminal(ctx context.Context, run sessionruntime.TerminalRun) {
	if r.store == nil {
		return
	}
	status, ok := contextLifecycleStatusForTerminalRun(run.State)
	if !ok {
		return
	}
	runUUID, botUUID, sessionUUID, err := parseIDs(run.RunID, run.BotID, run.SessionID)
	if err != nil {
		r.clearContextLifecycleCandidates(run.RunID)
		r.recordContextLifecyclePersistenceError(err, run.RunID, run.BotID, run.SessionID, status)
		return
	}

	writeCtx, cancel := contextLifecycleBoundedContext(ctx)
	defer cancel()
	existing, getErr := r.store.GetContextLifecycleByRunID(writeCtx, runUUID)
	existingReady := getErr == nil
	if getErr != nil && !errors.Is(getErr, pgx.ErrNoRows) {
		r.recordContextLifecyclePersistenceError(getErr, run.RunID, run.BotID, run.SessionID, status)
		return
	}
	if existingReady && (existing.BotID != botUUID || existing.SessionID != sessionUUID) {
		r.clearContextLifecycleCandidates(run.RunID)
		r.recordContextLifecyclePersistenceError(
			errors.New("existing context lifecycle identity does not match terminal run"),
			run.RunID,
			run.BotID,
			run.SessionID,
			status,
		)
		return
	}

	candidate, candidateReady := r.contextLifecycleCandidateFor(run)
	if candidateReady && (candidate.botID != strings.TrimSpace(run.BotID) || candidate.sessionID != strings.TrimSpace(run.SessionID)) {
		r.recordContextLifecyclePersistenceError(
			errors.New("context lifecycle candidate identity does not match terminal run"),
			run.RunID,
			run.BotID,
			run.SessionID,
			status,
		)
		candidateReady = false
	}
	var (
		snapshot        []byte
		replaceSnapshot bool
	)
	switch {
	case candidateReady && candidate.quality > CandidateMinimal:
		snapshot = append([]byte(nil), candidate.snapshot...)
		replaceSnapshot = true
	case existingReady:
		snapshot = append([]byte(nil), existing.Snapshot...)
	case !existingReady:
		recovered, ready, recoverErr := r.assistantContextLifecycleSnapshot(writeCtx, runUUID)
		if recoverErr != nil {
			r.recordContextLifecyclePersistenceError(recoverErr, run.RunID, run.BotID, run.SessionID, status)
			return
		}
		switch {
		case ready:
			snapshot = recovered
			replaceSnapshot = true
		case candidateReady:
			snapshot = append([]byte(nil), candidate.snapshot...)
		default:
			snapshot, err = json.Marshal(MinimalSnapshot())
			if err != nil {
				r.recordContextLifecyclePersistenceError(err, run.RunID, run.BotID, run.SessionID, status)
				return
			}
		}
	}

	defaultStatus := status
	refinedCode := ""
	switch {
	case candidateReady && candidate.status != defaultStatus &&
		contextLifecycleStatusCompatibleWithTerminalRun(run.State, candidate.status):
		status = candidate.status
	case existingReady && existing.Status != defaultStatus &&
		contextLifecycleStatusCompatibleWithTerminalRun(run.State, existing.Status):
		status = existing.Status
	default:
		status, refinedCode = refineTerminalContextLifecycleStatus(run, defaultStatus, snapshot)
	}

	errorCode := refinedCode
	if errorCode == "" {
		errorCode = terminalContextLifecycleErrorCode(run, status, candidate, candidateReady, existing, existingReady)
	}
	var code pgtype.Text
	if errorCode != "" {
		code = pgtype.Text{String: errorCode, Valid: true}
	}
	_, err = r.store.UpsertTerminalContextLifecycle(writeCtx, sqlc.UpsertTerminalContextLifecycleParams{
		RunID:            runUUID,
		BotID:            botUUID,
		SessionID:        sessionUUID,
		Status:           status,
		ErrorCode:        code,
		Snapshot:         snapshot,
		ReplaceSnapshot:  replaceSnapshot,
		ReplaceErrorCode: candidateReady && candidate.status == status && candidate.errorCode != "",
	})
	if err == nil {
		r.clearContextLifecycleCandidates(run.RunID)
		return
	}
	// A connection can fail after PostgreSQL committed the upsert. Verify the
	// authoritative identity and status before retaining the candidate for the
	// elected repair pass.
	confirmed, confirmErr := r.store.GetContextLifecycleByRunID(writeCtx, runUUID)
	if confirmErr == nil && confirmed.BotID == botUUID && confirmed.SessionID == sessionUUID && confirmed.Status == status {
		r.clearContextLifecycleCandidates(run.RunID)
		return
	}
	r.recordContextLifecyclePersistenceError(err, run.RunID, run.BotID, run.SessionID, status)
}

// ReconcileTerminals repairs the terminal runs that have no lifecycle row, one
// bounded batch per call.
func (r *Recorder) ReconcileTerminals(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	repairCtx, cancel := contextLifecycleBoundedContext(ctx)
	defer cancel()
	rows, err := r.store.ListTerminalSessionRunsNeedingContextLifecycle(
		repairCtx,
		contextLifecycleReconciliationBatchSize,
	)
	if err != nil {
		return fmt.Errorf("list terminal runs needing context lifecycle: %w", err)
	}
	for _, row := range rows {
		if err := repairCtx.Err(); err != nil {
			return fmt.Errorf("reconcile terminal context lifecycles: %w", err)
		}
		errorCode := ""
		if row.ErrorCode.Valid {
			errorCode = row.ErrorCode.String
		}
		r.ReconcileTerminal(repairCtx, sessionruntime.TerminalRun{
			RunID:        runreq.PGUUIDString(row.RunID),
			BotID:        runreq.PGUUIDString(row.BotID),
			SessionID:    runreq.PGUUIDString(row.SessionID),
			FencingToken: row.FencingToken,
			State:        row.State,
			ErrorCode:    errorCode,
		})
	}
	if err := repairCtx.Err(); err != nil {
		return fmt.Errorf("reconcile terminal context lifecycles: %w", err)
	}
	return nil
}

func contextLifecycleBoundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx = runreq.NonNilContext(ctx)
	timeout := WriteTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (r *Recorder) contextLifecycleCandidateFor(run sessionruntime.TerminalRun) (lifecycleCandidate, bool) {
	key := lifecycleCandidateKey{
		runID:        strings.TrimSpace(run.RunID),
		fencingToken: run.FencingToken,
	}
	r.candidatesMu.Lock()
	candidate, ok := r.candidates[key]
	r.candidatesMu.Unlock()
	if ok {
		candidate.snapshot = append([]byte(nil), candidate.snapshot...)
	}
	return candidate, ok
}

func (r *Recorder) clearContextLifecycleCandidates(runID string) {
	runID = strings.TrimSpace(runID)
	r.candidatesMu.Lock()
	for key := range r.candidates {
		if key.runID == runID {
			delete(r.candidates, key)
		}
	}
	r.candidatesMu.Unlock()
}

func contextLifecycleStatusForTerminalRun(state string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "completed":
		return StatusCompleted, true
	case "aborted":
		return StatusAborted, true
	case "failed", "lost":
		return StatusFailedProvider, true
	default:
		return "", false
	}
}

func contextLifecycleStatusCompatibleWithTerminalRun(state, status string) bool {
	state = strings.ToLower(strings.TrimSpace(state))
	status = strings.ToLower(strings.TrimSpace(status))
	switch state {
	case "completed":
		return status == StatusCompleted || status == StatusFallback
	case "failed":
		return status == StatusFailedProvider || status == StatusFailedBudget
	case "lost":
		return status == StatusFailedProvider
	case "aborted":
		return status == StatusAborted
	default:
		return false
	}
}

func terminalContextLifecycleErrorCode(
	run sessionruntime.TerminalRun,
	status string,
	candidate lifecycleCandidate,
	candidateReady bool,
	existing sqlc.GetContextLifecycleByRunIDRow,
	existingReady bool,
) string {
	if status != StatusFailedProvider && status != StatusFailedBudget {
		return ""
	}
	if candidateReady && candidate.status == status && candidate.errorCode != "" {
		return candidate.errorCode
	}
	if existingReady && existing.Status == status && existing.ErrorCode.Valid {
		return existing.ErrorCode.String
	}
	if status == StatusFailedProvider {
		return strings.TrimSpace(run.ErrorCode)
	}
	return ""
}

func lifecycleBudgetEvidence(snapshot contextfrag.LifecycleSnapshot) (budgetFailure bool, budgetReason string, fallback bool) {
	for _, mutation := range snapshot.Mutations {
		switch mutation.Kind {
		case contextfrag.MutationContextBudgetFailure:
			budgetFailure = true
			budgetReason = strings.TrimSpace(mutation.Detail)
		case contextfrag.MutationContextViewFallback:
			fallback = true
		}
	}
	return budgetFailure, budgetReason, fallback
}

// refineTerminalContextLifecycleStatus upgrades a state-derived terminal
// status with durable evidence recovered from the chosen snapshot and the
// run's stable error code, so fallback and failed_budget survive restarts
// that lost the in-memory candidate. It never crosses the run-state
// compatibility boundary.
func refineTerminalContextLifecycleStatus(
	run sessionruntime.TerminalRun,
	status string,
	snapshot []byte,
) (string, string) {
	state := strings.ToLower(strings.TrimSpace(run.State))
	if state != "completed" && state != "failed" {
		return status, ""
	}
	var budgetFailure, fallback bool
	var budgetReason string
	if decoded, err := contextfrag.DecodeLifecycleSnapshot(snapshot); err == nil {
		budgetFailure, budgetReason, fallback = lifecycleBudgetEvidence(decoded)
	}
	runCode := strings.TrimSpace(run.ErrorCode)
	switch state {
	case "completed":
		if fallback {
			return StatusFallback, ""
		}
	case "failed":
		switch {
		case runCode == string(apperror.CodeContextProtectedOverflow) ||
			runCode == string(apperror.CodeContextBudgetUnsatisfied):
			return StatusFailedBudget, runCode
		case budgetFailure && budgetReason == "protected_context_overflow":
			return StatusFailedBudget, string(apperror.CodeContextProtectedOverflow)
		case budgetFailure:
			return StatusFailedBudget, string(apperror.CodeContextBudgetUnsatisfied)
		}
	}
	return status, ""
}

func classifyContextLifecycleTerminal(
	ctx context.Context,
	snapshot contextfrag.LifecycleSnapshot,
	cause error,
) (string, string) {
	budgetFailure, budgetReason, fallback := lifecycleBudgetEvidence(snapshot)
	code := apperror.CodeOf(cause)
	if budgetFailure || code == apperror.CodeContextProtectedOverflow || code == apperror.CodeContextBudgetUnsatisfied {
		switch {
		case code == apperror.CodeContextProtectedOverflow, code == apperror.CodeContextBudgetUnsatisfied:
			return StatusFailedBudget, string(code)
		case budgetReason == "protected_context_overflow":
			return StatusFailedBudget, string(apperror.CodeContextProtectedOverflow)
		default:
			return StatusFailedBudget, string(apperror.CodeContextBudgetUnsatisfied)
		}
	}
	if explicitRunCancellation(ctx, cause) || drivenRunStopped(cause) {
		return StatusAborted, ""
	}
	if cause != nil {
		return StatusFailedProvider, string(code)
	}
	if fallback {
		return StatusFallback, ""
	}
	return StatusCompleted, ""
}

// explicitRunCancellation reports a user-driven cancellation: the run context
// was canceled with context.Canceled as the cause, and the terminal error is
// that cancellation rather than a failure the runtime canceled itself over
// (budget failures cancel with their own error as the cause).
func explicitRunCancellation(ctx context.Context, cause error) bool {
	return errors.Is(context.Cause(runreq.NonNilContext(ctx)), context.Canceled) &&
		(errors.Is(cause, context.Canceled) || errors.Is(apperror.CauseOf(cause), context.Canceled))
}

// drivenRunStopped reports a stop the run driver recorded. The driver cancels
// its execution context with outcome.ErrRunStopped rather than context.Canceled so a
// cancellation nobody asked for is not misread as a user stop; the lifecycle
// status still has to read "aborted", which is why the marker is checked here
// instead of widening explicitRunCancellation (that one also serves the loops
// the driver replaced).
func drivenRunStopped(cause error) bool {
	return errors.Is(cause, outcome.ErrRunStopped)
}

func contextLifecycleOwnershipLost(ctx context.Context, cause error) bool {
	return errors.Is(context.Cause(runreq.NonNilContext(ctx)), sessionruntime.ErrRunOwnershipLost) ||
		errors.Is(cause, sessionruntime.ErrRunOwnershipLost) ||
		errors.Is(apperror.CauseOf(cause), sessionruntime.ErrRunOwnershipLost) ||
		errors.Is(cause, sessionruntime.ErrCommandTargetNotActive) ||
		errors.Is(apperror.CauseOf(cause), sessionruntime.ErrCommandTargetNotActive)
}

func parseIDs(runID, botID, sessionID string) (pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
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

func (r *Recorder) recordContextLifecyclePersistenceError(
	err error,
	runID, botID, sessionID, status string,
) {
	count := r.persistenceErrors.Add(1)
	if r.logger == nil {
		return
	}
	r.logger.Error("persist context lifecycle failed",
		slog.Any("error", err),
		slog.String("run_id", runID),
		slog.String("bot_id", botID),
		slog.String("session_id", sessionID),
		slog.String("status", status),
		slog.Uint64("failure_count", count),
	)
}
