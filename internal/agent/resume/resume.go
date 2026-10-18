// Package resume continues the runs a server shutdown interrupted. A worker
// scans each team scope for interrupted runs, renews the credentials they
// carried, and drives each as a continuation of its session.
package resume

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/plan"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Queries is the database port of the interrupted-run scan.
type Queries interface {
	ListInterruptedSessionRuns(context.Context, pgtype.UUID) ([]dbsqlc.SessionRun, error)
	RetireSupersededInterruptedSessionRuns(context.Context) (int64, error)
	RetireInterruptedSessionRun(context.Context, pgtype.UUID) (int64, error)
}

// Accounts says whether the account a resumed credential names may still act.
type Accounts interface {
	ValidateSession(ctx context.Context, userID string) error
}

// Ready checks workspace transport before a continuation is claimed.
type Ready func(ctx context.Context, botID, targetID string) error

// Deps is everything New needs. Ready is optional; the rest is required.
type Deps struct {
	// Logger receives the worker's own records.
	Logger *slog.Logger
	// Queries lists and retires interrupted runs.
	Queries Queries
	// Accounts validates the account behind a renewed credential.
	Accounts Accounts
	// BotPermissions decides whether that account may still chat with the bot.
	BotPermissions runreq.BotPermissionChecker
	// Signer renews the credentials of a resumed run.
	Signer *admit.Signer
	// Admit admits the continuation.
	Admit *admit.Admitter
	// Decider says what the interrupted run's open decisions were.
	Decider *decide.Decider
	// Transcript finds the request row the continuation answers.
	Transcript *transcript.Reader
	// Driver executes the continuation.
	Driver *drive.Driver
	// Plan plans the continuation.
	Plan *plan.Planner
	// TeamScopes names the scopes the worker scans.
	TeamScopes runreq.TeamScopes
	// Ready checks workspace transport before a continuation is claimed;
	// without it the check passes.
	Ready Ready
}

// Worker finds and resumes interrupted runs.
type Worker struct {
	logger      *slog.Logger
	queries     Queries
	accounts    Accounts
	permissions runreq.BotPermissionChecker
	signer      *admit.Signer
	admit       *admit.Admitter
	decider     *decide.Decider
	transcript  *transcript.Reader
	driver      *drive.Driver
	plan        *plan.Planner
	teamScopes  runreq.TeamScopes
	ready       Ready
	stop        context.CancelFunc
	done        chan struct{}
}

// New returns a Worker.
func New(d Deps) *Worker {
	ready := d.Ready
	if ready == nil {
		ready = func(context.Context, string, string) error { return nil }
	}
	return &Worker{
		logger:      d.Logger,
		queries:     d.Queries,
		accounts:    d.Accounts,
		permissions: d.BotPermissions,
		signer:      d.Signer,
		admit:       d.Admit,
		decider:     d.Decider,
		transcript:  d.Transcript,
		driver:      d.Driver,
		plan:        d.Plan,
		teamScopes:  d.TeamScopes,
		ready:       ready,
	}
}
