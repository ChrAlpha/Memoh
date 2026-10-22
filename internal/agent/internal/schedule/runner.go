// Package schedule runs a scheduled command as an agent turn. The schedule
// service decides when a fire is due; this package admits it, runs it
// unattended through the session's runtime, and answers in the vocabulary the
// schedule service reads.
package schedule

import (
	"github.com/felinics/memoh/internal/agent/internal/admit"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/externalrun"
	"github.com/felinics/memoh/internal/agent/internal/plan"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
)

// Deps is everything New needs. Every field is required.
type Deps struct {
	// Admit admits the fire as a run of its session.
	Admit *admit.Admitter
	// ActiveTurns counts the fires in flight, for shutdown.
	ActiveTurns *runreq.ActiveTurnTracker
	// ExternalRun says which runtime the session executes on.
	ExternalRun *externalrun.Host
	// Driver executes the run and writes its terminal state.
	Driver *drive.Driver
	// Plan plans the run.
	Plan *plan.Planner
}

// Runner executes scheduled fires.
type Runner struct {
	admit       *admit.Admitter
	activeTurns *runreq.ActiveTurnTracker
	externalrun *externalrun.Host
	driver      *drive.Driver
	plan        *plan.Planner
}

// New returns a Runner.
func New(d Deps) *Runner {
	return &Runner{
		admit:       d.Admit,
		activeTurns: d.ActiveTurns,
		externalrun: d.ExternalRun,
		driver:      d.Driver,
		plan:        d.Plan,
	}
}
