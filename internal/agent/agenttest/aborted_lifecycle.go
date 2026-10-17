package agenttest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

// AbortedLifecycleQueries is the database double behind the abort
// reconciliation: it serves one aborted run, the lifecycle row and assistant
// metadata the test configures, and a pending-decision lookup that clears after
// a set number of reads. Set the configuration fields before the run starts.
type AbortedLifecycleQueries struct {
	dbstore.Queries
	mu       sync.Mutex
	getCalls int

	// Existing is the lifecycle row, visible from the ExistingAfter-th read on
	// (immediately when zero).
	Existing      *sqlc.GetContextLifecycleByRunIDRow
	ExistingAfter int
	// AssistantID and Metadata are the assistant message the snapshot is
	// recovered from.
	AssistantID pgtype.UUID
	Metadata    []byte
	MetadataErr error
	// SessionRun is what the run ledger answers.
	SessionRun    sqlc.SessionRun
	SessionRunErr error
	// Pending makes decisions pending until PendingUntil reads were made
	// (always when PendingUntil is zero).
	Pending      bool
	PendingUntil int
	UpsertErr    error
	// UpsertCh receives a token for every aborted upsert.
	UpsertCh chan struct{}

	pendingReads int
	upserts      []sqlc.UpsertAbortedContextLifecycleParams
}

// NewAbortedLifecycleQueries returns a double whose run is aborted and
// identified by the Lifecycle* constants.
func NewAbortedLifecycleQueries(t *testing.T) *AbortedLifecycleQueries {
	t.Helper()
	runID, err := db.ParseUUID(LifecycleRunID)
	if err != nil {
		t.Fatal(err)
	}
	botID, err := db.ParseUUID(LifecycleBotID)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := db.ParseUUID(LifecycleSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return &AbortedLifecycleQueries{
		SessionRun: sqlc.SessionRun{RunID: runID, BotID: botID, SessionID: sessionID, State: "aborted"},
		UpsertCh:   make(chan struct{}, 1),
	}
}

func (*AbortedLifecycleQueries) CreateContextLifecycle(
	context.Context,
	sqlc.CreateContextLifecycleParams,
) (sqlc.CreateContextLifecycleRow, error) {
	return sqlc.CreateContextLifecycleRow{}, errors.New("unexpected lifecycle create")
}

func (q *AbortedLifecycleQueries) GetContextLifecycleByRunID(
	context.Context,
	pgtype.UUID,
) (sqlc.GetContextLifecycleByRunIDRow, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.getCalls++
	if q.Existing == nil || q.ExistingAfter > 0 && q.getCalls < q.ExistingAfter {
		return sqlc.GetContextLifecycleByRunIDRow{}, pgx.ErrNoRows
	}
	return *q.Existing, nil
}

func (q *AbortedLifecycleQueries) GetLatestAssistantContextLifecycleMetadataByRunID(
	context.Context,
	pgtype.UUID,
) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.MetadataErr != nil {
		return nil, q.MetadataErr
	}
	if q.Metadata == nil {
		return nil, pgx.ErrNoRows
	}
	return append([]byte(nil), q.Metadata...), nil
}

func (q *AbortedLifecycleQueries) GetLatestAssistantContextLifecycleByRunID(
	context.Context,
	pgtype.UUID,
) (sqlc.GetLatestAssistantContextLifecycleByRunIDRow, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.MetadataErr != nil {
		return sqlc.GetLatestAssistantContextLifecycleByRunIDRow{}, q.MetadataErr
	}
	if q.Metadata == nil {
		return sqlc.GetLatestAssistantContextLifecycleByRunIDRow{}, pgx.ErrNoRows
	}
	return sqlc.GetLatestAssistantContextLifecycleByRunIDRow{
		ID:       q.AssistantID,
		Metadata: append([]byte(nil), q.Metadata...),
	}, nil
}

func (*AbortedLifecycleQueries) UpdateAbortedContextLifecycleSnapshot(
	context.Context,
	sqlc.UpdateAbortedContextLifecycleSnapshotParams,
) (sqlc.UpdateAbortedContextLifecycleSnapshotRow, error) {
	return sqlc.UpdateAbortedContextLifecycleSnapshotRow{}, errors.New("unexpected lifecycle update")
}

func (q *AbortedLifecycleQueries) UpsertAbortedContextLifecycle(
	_ context.Context,
	arg sqlc.UpsertAbortedContextLifecycleParams,
) (sqlc.UpsertAbortedContextLifecycleRow, error) {
	q.mu.Lock()
	q.upserts = append(q.upserts, arg)
	q.mu.Unlock()
	if q.UpsertCh != nil {
		select {
		case q.UpsertCh <- struct{}{}:
		default:
		}
	}
	return sqlc.UpsertAbortedContextLifecycleRow{}, q.UpsertErr
}

func (q *AbortedLifecycleQueries) GetSessionRun(context.Context, pgtype.UUID) (sqlc.SessionRun, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.SessionRun, q.SessionRunErr
}

func (q *AbortedLifecycleQueries) ListPendingToolApprovalsByRun(
	context.Context,
	pgtype.UUID,
) ([]sqlc.ToolApprovalRequest, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pendingReads++
	if !q.Pending || q.PendingUntil > 0 && q.pendingReads > q.PendingUntil {
		return nil, nil
	}
	return []sqlc.ToolApprovalRequest{{}}, nil
}

func (*AbortedLifecycleQueries) ListPendingUserInputsByRun(
	context.Context,
	pgtype.UUID,
) ([]sqlc.UserInputRequest, error) {
	return nil, nil
}

// PendingRuntimeDecisions answers the pending-decision question the way the
// service does from the two queries above, so a test can hand the double to the
// reconciliation directly.
func (q *AbortedLifecycleQueries) PendingRuntimeDecisions(ctx context.Context, _ string) ([]sessionruntime.DecisionTarget, error) {
	approvals, err := q.ListPendingToolApprovalsByRun(ctx, pgtype.UUID{})
	if err != nil {
		return nil, err
	}
	return make([]sessionruntime.DecisionTarget, len(approvals)), nil
}

// RecordedUpserts returns the aborted upserts written so far.
func (q *AbortedLifecycleQueries) RecordedUpserts() []sqlc.UpsertAbortedContextLifecycleParams {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]sqlc.UpsertAbortedContextLifecycleParams(nil), q.upserts...)
}

// PendingReads counts the pending-decision lookups made so far.
func (q *AbortedLifecycleQueries) PendingReads() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pendingReads
}

// WaitForUpsert blocks until an aborted upsert is written.
func (q *AbortedLifecycleQueries) WaitForUpsert(t *testing.T) {
	t.Helper()
	select {
	case <-q.UpsertCh:
	case <-time.After(2 * time.Second):
		t.Fatal("aborted lifecycle reconciliation did not write")
	}
}
