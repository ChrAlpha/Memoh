// Package plan turns an admitted run into the plan the driver executes. An
// entry hands it what it admitted (a chat turn, a schedule fire, a decision
// that was answered) and it resolves the run's context, binds the step
// commits and the compaction between model calls, and builds the executor.
// Every run the built-in agent executes ends in the same tail; the entries
// differ only in the options they give it.
package plan

import (
	"context"
	"log/slog"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/title"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Agent is the part of the built-in agent a run needs: it streams a turn, and
// it runs one approved tool call outside a stream.
type Agent interface {
	nativeStreamer
	ExecuteToolWithUIMetadata(ctx context.Context, cfg native.RunConfig, call sdk.ToolCall) (sdk.ToolResultPart, map[string]any, error)
}

// Store is the database port of the session's model preference.
type Store interface {
	UpdateSessionModelPreference(ctx context.Context, arg sqlc.UpdateSessionModelPreferenceParams) error
}

// StepOffsets tells how many steps of a parked run are already committed, so a
// continuation numbers its own steps after them.
type StepOffsets interface {
	ContinuationStepIndex(handle sessionruntime.RunHandle) (int, error)
}

// ExecutionMarker records that the call a decision approved is about to run.
// It marks the decision row executing under the run's fence, once: a second
// claim, or a claim by a run that no longer owns the decision, fails with
// sessionruntime.ErrRunOwnershipLost. A context without a fence marks nothing.
type ExecutionMarker interface {
	MarkExecuting(ctx context.Context, runID, decisionID string) error
}

// Deps is everything New needs. Every field is required.
type Deps struct {
	// Logger receives the planner's own records; nil discards them.
	Logger *slog.Logger
	// Store writes the session's model preference back.
	Store Store
	// Agent streams the turn and runs approved tools.
	Agent Agent
	// Runtime reports the committed steps of a parked run.
	Runtime StepOffsets
	// Executions marks an approved call executing.
	Executions ExecutionMarker
	// Admit records the intent a later process resumes a run from.
	Admit *admit.Admitter
	// Bot runs the user-message hook.
	Bot *botenv.Env
	// Transcript assembles the history a continuation resumes from.
	Transcript *transcript.Reader
	// Lifecycle records the context lifecycle row of the run's end.
	Lifecycle *lifecycle.Recorder
	// Persist writes the run's history and builds its step committer.
	Persist *persist.Writer
	// Compact binds the compaction between model calls.
	Compact *compact.Compactor
	// Runconfig resolves the run's context.
	Runconfig *runconfig.Builder
	// External plans a run an External Agent runtime executes.
	External *externalrun.Host
	// Title names a new session from its first message.
	Title *title.Generator
}

// Planner builds the plan of an admitted run.
type Planner struct {
	logger      *slog.Logger
	store       Store
	agent       Agent
	runtime     StepOffsets
	executions  ExecutionMarker
	admit       *admit.Admitter
	bot         *botenv.Env
	transcript  *transcript.Reader
	lifecycle   *lifecycle.Recorder
	persist     *persist.Writer
	compact     *compact.Compactor
	runconfig   *runconfig.Builder
	externalrun *externalrun.Host
	title       *title.Generator
}

// New returns a Planner.
func New(d Deps) *Planner {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Planner{
		logger:      logger,
		store:       d.Store,
		agent:       d.Agent,
		runtime:     d.Runtime,
		executions:  d.Executions,
		admit:       d.Admit,
		bot:         d.Bot,
		transcript:  d.Transcript,
		lifecycle:   d.Lifecycle,
		persist:     d.Persist,
		compact:     d.Compact,
		runconfig:   d.Runconfig,
		externalrun: d.External,
		title:       d.Title,
	}
}
