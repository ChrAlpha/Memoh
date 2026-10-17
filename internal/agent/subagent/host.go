// Package subagent hosts the runs of spawned agents: it admits a spawned
// agent's turn, commits the steps of the run to its thread as they complete,
// publishes the run's events, compacts between model calls and records what
// resumes the run.
package subagent

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// Events is the session runtime's intake of agent events. It answers with the
// status the run has after the event.
type Events interface {
	HandleAgentEventWithStatus(ctx context.Context, handle sessionruntime.RunHandle, event native.StreamEvent) ([]chatview.UIMessage, string, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the host's own records.
	Logger *slog.Logger
	// Admit admits a spawned agent's turn and records what resumes it.
	Admit *admit.Admitter
	// Driver writes the terminal state of a run that ended.
	Driver *drive.Driver
	// Lifecycle records the context lifecycle of a finished run.
	Lifecycle *lifecycle.Recorder
	// Compact compacts a run's context between model calls; without a
	// compaction engine it declines.
	Compact *compact.Compactor
	// ActiveTurns keeps the process open until the run's persistence is done.
	ActiveTurns *runreq.ActiveTurnTracker
	// Events receives the run's events. A host without it publishes nothing.
	Events Events
	// Steps persists one fenced step at a time. A host without it declines
	// incremental persistence, and the terminal snapshot writes the run.
	Steps messagepkg.AgentStepPersister
}

// Host is the run host of spawned agents.
type Host struct {
	logger      *slog.Logger
	admit       *admit.Admitter
	driver      *drive.Driver
	lifecycle   *lifecycle.Recorder
	compact     *compact.Compactor
	activeTurns *runreq.ActiveTurnTracker
	events      Events
	steps       messagepkg.AgentStepPersister
}

// New returns a Host.
func New(d Deps) *Host {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Host{
		logger:      logger,
		admit:       d.Admit,
		driver:      d.Driver,
		lifecycle:   d.Lifecycle,
		compact:     d.Compact,
		activeTurns: d.ActiveTurns,
		events:      d.Events,
		steps:       d.Steps,
	}
}
