package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

const (
	abortedContextLifecycleRetryInterval    = 25 * time.Millisecond
	abortedContextLifecycleMetadataFallback = 500 * time.Millisecond
)

type assistantContextLifecycleStore interface {
	GetLatestAssistantContextLifecycleByRunID(
		context.Context,
		pgtype.UUID,
	) (sqlc.GetLatestAssistantContextLifecycleByRunIDRow, error)
}

// ReconcileAborted writes the lifecycle row of an aborted run. It waits for the
// abort to land in the run ledger and for the run's pending decisions to
// settle, then falls back to the assistant metadata and finally to a minimal
// snapshot.
func (r *Recorder) ReconcileAborted(
	ctx context.Context,
	runID, botID, sessionID string,
) {
	runUUID, botUUID, sessionUUID, err := parseIDs(runID, botID, sessionID)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, StatusAborted)
		return
	}

	var (
		pendingErr         error
		pendingKnown       bool
		pendingDecision    bool
		metadataFallbackAt time.Time
	)
	for {
		run, runErr := r.runs.GetSessionRun(ctx, runUUID)
		switch {
		case runErr == nil:
			if run.BotID != botUUID || run.SessionID != sessionUUID {
				r.recordContextLifecyclePersistenceError(
					errors.New("aborted session run identity does not match request"),
					runID,
					botID,
					sessionID,
					StatusAborted,
				)
				return
			}
			state := ledger.State(run.State)
			if state.Terminal() && state != ledger.StateAborted {
				return
			}
			if state == ledger.StateAborted {
				snapshot, ready, snapshotErr := r.existingAbortedContextLifecycleSnapshot(ctx, runUUID)
				if snapshotErr != nil {
					r.recordContextLifecyclePersistenceError(snapshotErr, runID, botID, sessionID, StatusAborted)
					return
				}
				if ready {
					r.upsertAbortedContextLifecycle(ctx, runID, botID, sessionID, runUUID, botUUID, sessionUUID, snapshot)
					return
				}

				pendingTargets, decisionErr := r.decisions.PendingRuntimeDecisions(ctx, runID)
				if decisionErr != nil {
					r.recordContextLifecyclePersistenceError(decisionErr, runID, botID, sessionID, StatusAborted)
					return
				}
				currentPending := len(pendingTargets) > 0
				if !pendingKnown || pendingDecision && !currentPending {
					metadataFallbackAt = time.Now().Add(abortedContextLifecycleMetadataFallback)
				}
				pendingKnown = true
				pendingDecision = currentPending
				if !metadataFallbackAt.IsZero() && !time.Now().Before(metadataFallbackAt) {
					snapshot, ready, snapshotErr = r.assistantContextLifecycleSnapshot(ctx, runUUID)
					if snapshotErr != nil {
						r.recordContextLifecyclePersistenceError(snapshotErr, runID, botID, sessionID, StatusAborted)
						return
					}
					if !ready {
						snapshot, snapshotErr = json.Marshal(MinimalSnapshot())
						if snapshotErr != nil {
							r.recordContextLifecyclePersistenceError(snapshotErr, runID, botID, sessionID, StatusAborted)
							return
						}
					}
					r.upsertAbortedContextLifecycle(ctx, runID, botID, sessionID, runUUID, botUUID, sessionUUID, snapshot)
					return
				}
				pendingErr = errors.New("aborted run has no recoverable context lifecycle snapshot")
			} else {
				pendingErr = fmt.Errorf("session run has not reached aborted state: %s", state)
			}
		case errors.Is(runErr, pgx.ErrNoRows):
			pendingErr = errors.New("aborted session run is not visible")
		default:
			r.recordContextLifecyclePersistenceError(runErr, runID, botID, sessionID, StatusAborted)
			return
		}

		if !waitContextLifecycleRetry(ctx) {
			if pendingErr == nil {
				pendingErr = context.Cause(ctx)
			}
			r.recordContextLifecyclePersistenceError(pendingErr, runID, botID, sessionID, StatusAborted)
			return
		}
	}
}

func (r *Recorder) existingAbortedContextLifecycleSnapshot(
	ctx context.Context,
	runID pgtype.UUID,
) ([]byte, bool, error) {
	existing, err := r.store.GetContextLifecycleByRunID(ctx, runID)
	if err == nil {
		return existing.Snapshot, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	return nil, false, nil
}

func (r *Recorder) assistantContextLifecycleSnapshot(
	ctx context.Context,
	runID pgtype.UUID,
) ([]byte, bool, error) {
	var (
		assistantID pgtype.UUID
		metadata    []byte
		err         error
	)
	if store, ok := r.store.(assistantContextLifecycleStore); ok {
		row, rowErr := store.GetLatestAssistantContextLifecycleByRunID(ctx, runID)
		assistantID = row.ID
		metadata = row.Metadata
		err = rowErr
	} else {
		metadata, err = r.store.GetLatestAssistantContextLifecycleMetadataByRunID(ctx, runID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw, ok := contextfrag.LifecycleSnapshotRawFromMetadata(metadata)
	if !ok {
		return nil, false, nil
	}
	if assistantID.Valid {
		stamped, stampErr := contextfrag.StampLifecycleAssistantMessageID(raw, assistantID.String())
		if stampErr != nil {
			return nil, false, stampErr
		}
		raw = stamped
	}
	return raw, true, nil
}

func (r *Recorder) upsertAbortedContextLifecycle(
	ctx context.Context,
	runID, botID, sessionID string,
	runUUID, botUUID, sessionUUID pgtype.UUID,
	snapshot []byte,
) {
	_, err := r.store.UpsertAbortedContextLifecycle(
		ctx,
		sqlc.UpsertAbortedContextLifecycleParams{
			RunID:     runUUID,
			BotID:     botUUID,
			SessionID: sessionUUID,
			Snapshot:  snapshot,
		},
	)
	if err != nil {
		r.recordContextLifecyclePersistenceError(err, runID, botID, sessionID, StatusAborted)
	}
}

func waitContextLifecycleRetry(ctx context.Context) bool {
	timer := time.NewTimer(abortedContextLifecycleRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
