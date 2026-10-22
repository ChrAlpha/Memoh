// Package web runs the sessions a person drives from the Web client: a send, a
// retry of the latest turn, an edit of it. It admits the submission against the
// session runtime (or parks it in the session's queue when the session is
// busy), writes what the user said ahead of the run, and executes the run.
package web

import (
	"github.com/felinics/memoh/internal/agent/internal/admit"
	"github.com/felinics/memoh/internal/agent/internal/botenv"
	"github.com/felinics/memoh/internal/agent/internal/compact"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/pending"
	"github.com/felinics/memoh/internal/agent/internal/persist"
	"github.com/felinics/memoh/internal/agent/internal/plan"
	"github.com/felinics/memoh/internal/agent/internal/runconfig"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// Deps is everything New needs. Every field is required.
type Deps struct {
	// Runtime is the durable admission gate of the session runtime.
	Runtime runreq.TurnAdmitter
	// Admit fences the run an admission started.
	Admit *admit.Admitter
	// ActiveTurns counts the runs the component starts, for shutdown.
	ActiveTurns *runreq.ActiveTurnTracker
	// Pending is where a send that meets a busy session waits.
	Pending *pending.Inputs
	// Compact holds the session's compaction back while a run appends to it.
	Compact *compact.Compactor
	// Driver executes the run and writes its terminal state.
	Driver *drive.Driver
	// Plan plans the run.
	Plan *plan.Planner
	// Messages is the history a retry or an edit replaces.
	Messages messagepkg.Service
	// Persist writes the user's turn and the replacement.
	Persist *persist.Writer
	// Runconfig binds the request's workspace.
	Runconfig *runconfig.Builder
	// Bot runs the user-message hook.
	Bot *botenv.Env
}

// Runner admits and runs Web submissions.
type Runner struct {
	runtime     runreq.TurnAdmitter
	admit       *admit.Admitter
	activeTurns *runreq.ActiveTurnTracker
	pending     *pending.Inputs
	compact     *compact.Compactor
	driver      *drive.Driver
	plan        *plan.Planner
	messages    messagepkg.Service
	persist     *persist.Writer
	runconfig   *runconfig.Builder
	bot         *botenv.Env
}

// New returns a Runner.
func New(d Deps) *Runner {
	return &Runner{
		runtime:     d.Runtime,
		admit:       d.Admit,
		activeTurns: d.ActiveTurns,
		pending:     d.Pending,
		compact:     d.Compact,
		driver:      d.Driver,
		plan:        d.Plan,
		messages:    d.Messages,
		persist:     d.Persist,
		runconfig:   d.Runconfig,
		bot:         d.Bot,
	}
}
