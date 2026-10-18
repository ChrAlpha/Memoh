// Package lifecycle records how the context of each run ended: the diagnostic
// snapshot of what the model was shown, its terminal status, and the repair of
// rows a crash or a lost run left unwritten.
package lifecycle

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgtype"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Runs reads the durable run ledger.
type Runs interface {
	GetSessionRun(context.Context, pgtype.UUID) (sqlc.SessionRun, error)
}

// Decisions lists the durable decisions that parked a run. The abort
// reconciliation waits for them to settle before it settles for the message
// metadata.
type Decisions interface {
	PendingRuntimeDecisions(ctx context.Context, runID string) ([]sessionruntime.DecisionTarget, error)
}

// Deps is everything New needs.
type Deps struct {
	// Store reads and writes the lifecycle rows.
	Store Store
	// Runs reads the run ledger to learn whether an abort has landed.
	Runs Runs
	// Decisions reports whether a run still has a decision waiting.
	Decisions Decisions
	// Logger receives persistence failures; they are counted and logged, never
	// returned.
	Logger *slog.Logger
}

// Recorder writes context lifecycle rows. Candidates staged during a run wait
// in memory until the run's terminal transition is accepted.
type Recorder struct {
	store     Store
	runs      Runs
	decisions Decisions
	logger    *slog.Logger

	persistenceErrors atomic.Uint64
	candidatesMu      sync.Mutex
	candidates        map[lifecycleCandidateKey]lifecycleCandidate
}

// New returns a Recorder.
func New(d Deps) *Recorder {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Recorder{
		store:     d.Store,
		runs:      d.Runs,
		decisions: d.Decisions,
		logger:    logger,
	}
}

// PersistenceErrors counts the writes that failed since the Recorder was built.
func (r *Recorder) PersistenceErrors() uint64 {
	return r.persistenceErrors.Load()
}
