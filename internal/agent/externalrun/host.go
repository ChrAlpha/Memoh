// Package externalrun runs turns on out-of-process runtimes (ACP, codex,
// claude-code): the registry of their drivers and the dispatch to one, the
// executor that prompts a driver and writes the round it reports, and the
// control surface of a runtime session (controls, steering, fork, model
// preference, abort).
package externalrun

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Store is the part of the database port the host reads and writes. The round
// reconciliation also needs transactions, which the store provides when it
// implements InTx.
type Store interface {
	GetSessionByID(ctx context.Context, id pgtype.UUID) (sqlc.BotSession, error)
	GetTurnAgentTurnID(ctx context.Context, arg sqlc.GetTurnAgentTurnIDParams) (string, error)
	UpdateSessionModelPreference(ctx context.Context, arg sqlc.UpdateSessionModelPreferenceParams) error
	DeleteRuntimeDecisionProjectionsByRun(ctx context.Context, arg sqlc.DeleteRuntimeDecisionProjectionsByRunParams) (int64, error)
}

// Admitter is the admission gate of the session runtime.
type Admitter interface {
	Admit(context.Context, sessionruntime.AdmitInput) (sessionruntime.Admission, error)
	// MarkInlineDecisionRun declares that an admitted run's runtime blocks
	// inline on decisions, so terminal decision statuses resume the run.
	MarkInlineDecisionRun(botID, sessionID, runID string)
}

// Aborter routes the abort of a control to the durable runtime.
type Aborter interface {
	AbortControl(ctx context.Context, botID, sessionID, runID, controlID string) (bool, error)
}

// Deps is everything New needs. Every field except Drivers and AllowedTeam is
// required.
type Deps struct {
	// Logger receives the host's own records.
	Logger *slog.Logger
	// Store is the database port.
	Store Store
	// Sessions reads sessions and merges runtime metadata into them.
	Sessions ports.SessionService
	// Messages persists the user message and decision rows of a turn.
	Messages messagepkg.Service
	// BotPermissions decides what an actor may do with a runtime session.
	BotPermissions ports.BotPermissionChecker
	// Admitter admits a runtime control as a run and declares that a turn's
	// decisions are answered inline.
	Admitter Admitter
	// Aborter acknowledges an abort control.
	Aborter Aborter
	// Runtime is the durable session runtime: run snapshots, aborts and the
	// steering queue.
	Runtime *sessionruntime.Manager
	// Persist writes the round and the failure rows.
	Persist *persist.Writer
	// Memory extracts memory from a persisted round.
	Memory *memory.Keeper
	// Runconfig prepares attachments, the workspace and the context sections.
	Runconfig *runconfig.Builder
	// Decider answers what a runtime asks the user to decide.
	Decider *decide.Decider
	// ActivePrompts is the registry the executor publishes its live output to.
	ActivePrompts *activeprompt.Registry
	// Driver writes the terminal state of a control run.
	Driver *drive.Driver
	// Lifecycle reconciles the context lifecycle of an aborted run.
	Lifecycle *lifecycle.Recorder

	// Drivers are the out-of-process runtime drivers. A session whose runtime
	// type has no driver fails with a stable "runtime unavailable" error.
	Drivers []external.Driver
	// AllowedTeam restricts runtime controls to one team; empty means no
	// restriction.
	AllowedTeam string
}

// Host runs turns on external runtime drivers and serves their controls.
type Host struct {
	logger      *slog.Logger
	store       Store
	sessions    ports.SessionService
	messages    messagepkg.Service
	permissions ports.BotPermissionChecker
	admitter    Admitter
	aborter     Aborter
	runtime     *sessionruntime.Manager
	persist     *persist.Writer
	memory      *memory.Keeper
	runconfig   *runconfig.Builder
	decider     *decide.Decider
	prompts     *activeprompt.Registry
	driver      *drive.Driver
	lifecycle   *lifecycle.Recorder
	drivers     map[string]external.Driver
	allowedTeam string
}

// New returns a Host.
func New(d Deps) *Host {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Host{
		logger:      logger,
		store:       d.Store,
		sessions:    d.Sessions,
		messages:    d.Messages,
		permissions: d.BotPermissions,
		admitter:    d.Admitter,
		aborter:     d.Aborter,
		runtime:     d.Runtime,
		persist:     d.Persist,
		memory:      d.Memory,
		runconfig:   d.Runconfig,
		decider:     d.Decider,
		prompts:     d.ActivePrompts,
		driver:      d.Driver,
		lifecycle:   d.Lifecycle,
		drivers:     driverMap(d.Drivers),
		allowedTeam: d.AllowedTeam,
	}
}

// driverMap indexes drivers by runtime type; a later driver replaces an
// earlier one of the same type.
func driverMap(drivers []external.Driver) map[string]external.Driver {
	byType := map[string]external.Driver{}
	for _, driver := range drivers {
		if driver == nil {
			continue
		}
		byType[strings.TrimSpace(driver.RuntimeType())] = driver
	}
	return byType
}
