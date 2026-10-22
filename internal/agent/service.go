// Package agent is the agent application service: the facade the transports
// call, the turn port the channel side talks to, and New, which assembles the
// components under internal/agent/* and registers the session runtime's
// callbacks. The work itself lives in those components.
package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/background"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/plan"
	"github.com/felinics/memoh/internal/agent/resume"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/schedule"
	"github.com/felinics/memoh/internal/agent/subagent"
	"github.com/felinics/memoh/internal/agent/title"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/agent/web"
	messageevent "github.com/felinics/memoh/internal/chat/event"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/hooks"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

// Service is the facade over the agent's components. Its exported methods
// delegate to one component each; the turn port (StartTurn, StopTurn,
// AdmitDiscuss and the answers) is implemented here because it speaks the
// channel side's vocabulary.
type Service struct {
	allowedTeam string

	activeTurns   *runreq.ActiveTurnTracker
	activePrompts *activeprompt.Registry
	botenv        *botenv.Env
	transcript    *transcript.Reader
	lifecycle     *lifecycle.Recorder
	persist       *persist.Writer
	driver        *drive.Driver
	admit         *admit.Admitter
	signer        *admit.Signer
	runconfig     *runconfig.Builder
	subagent      *subagent.Host
	externalrun   *externalrun.Host
	title         *title.Generator
	decider       *decide.Decider
	memory        *memory.Keeper
	overview      *memory.Overview
	compact       *compact.Compactor
	plan          *plan.Planner
	pending       *pending.Inputs
	web           *web.Runner
	schedule      *schedule.Runner
	continuer     *resume.Continuer
	resume        *resume.Scanner
	answer        *answer.Responder
	discussSweep  *pending.DiscussSweep
	queueSweep    *pending.QueueSweep
}

// Deps is everything New wires into a Service. The required block is checked
// by New; the optional block may stay zero, which turns the feature it names
// off; the configuration block carries plain values.
type Deps struct {
	// Required.

	// Logger receives the service's own records.
	Logger *slog.Logger
	// Queries is the database port used by every persistence path.
	Queries dbstore.Queries
	// Messages persists and reads chat messages.
	Messages messagepkg.Service
	// Sessions reads and updates session metadata and titles.
	Sessions runreq.SessionService
	// Settings resolves per-bot settings.
	Settings *settings.Service
	// Accounts resolves the account behind a request.
	Accounts *accounts.Service
	// Models resolves model and provider configuration.
	Models *models.Service
	// Agent is the in-process model runtime.
	Agent *native.Agent
	// SessionRuntime is the durable admission gate. Every turn entry point goes
	// through it, so a service without it starts nothing. New also registers the
	// service's runtime callbacks on it.
	SessionRuntime *sessionruntime.Manager
	// BotPermissions decides whether an account may act on a bot.
	BotPermissions runreq.BotPermissionChecker
	// WorkspaceTargets resolves a request-scoped Computer.
	WorkspaceTargets runconfig.WorkspaceTargetResolver
	// ToolApproval stores and resolves tool approval decisions.
	ToolApproval *toolapproval.Service
	// UserInput stores and resolves user input decisions.
	UserInput *userinput.Service
	// RunCredentialSecret signs the credentials of runs the server starts itself
	// and of resumed sessions.
	RunCredentialSecret string

	// Optional.

	// Memory provides memory search and write-back; without it runs have no memory.
	Memory *memprovider.Registry
	// Hooks runs user-defined hooks; without it no hook fires.
	Hooks *hooks.Service
	// Skills lists a bot's usable skills; without it requests carry none.
	Skills runconfig.SkillLoader
	// Assets inlines attachment content for the gateway; without it attachments
	// keep their stored references.
	Assets runconfig.GatewayAssetLoader
	// PlatformIdentities supplies connected platform accounts for the system
	// prompt; without it the prompt names none.
	PlatformIdentities runconfig.PlatformIdentitySource
	// Workdirs resolves session workdir bindings; without it bindings are ignored.
	Workdirs runconfig.WorkdirResolver
	// Compaction runs context compaction; without it nothing is compacted.
	Compaction *compaction.Service
	// Background supplies background task summaries; without it there are none.
	Background *background.Manager
	// Events broadcasts session events such as title updates; without it none
	// are sent.
	Events messageevent.Publisher
	// OutputMedia stores attachments that runs emit; without it an attachment
	// keeps the path it arrived with.
	OutputMedia drive.OutputMediaStore
	// OutputSpeech synthesizes spoken replies; without it a speech event is dropped.
	OutputSpeech drive.OutputSpeech
	// QueuedInputs is the durable queue for input that meets a busy session;
	// without it such input is refused as busy.
	QueuedInputs *sessionruntime.QueuedInputs
	// ExternalRuntimes are the out-of-process runtime drivers (ACP, codex,
	// claude-code). A session whose runtime type has no driver fails with a
	// stable "runtime unavailable" error.
	ExternalRuntimes []external.Driver
	// WorkspaceReadiness checks that the workspace a run would start on can be
	// reached. It is asked before work the system owes a session starts (an
	// interrupted run's continuation, a queued follow-up, an uncovered discuss
	// batch), never before a turn the user starts directly; without it the
	// workspace is always ready.
	WorkspaceReadiness pending.WorkspaceReadiness
	// TeamScopes is the tenancy provider of a hosted deployment; without it
	// resume and the discuss sweep cover only AllowedTeam.
	TeamScopes runreq.TeamScopes

	// Configuration.

	// ClockLocation is the time zone prompts render in; nil means UTC.
	ClockLocation *time.Location
	// ContextFallbackTokens is the context budget for models without a
	// configured window; zero keeps the shared default.
	ContextFallbackTokens int
	// AllowedTeam restricts the service to one team: the in-process runtime's
	// database pool is bound to a single team, so commands for any other team
	// fail closed (turn.ErrTeamNotServed). Empty means no restriction.
	AllowedTeam string
}

// validate names the first required dependency that is missing.
func (d Deps) validate() error {
	required := []struct {
		name    string
		missing bool
	}{
		{"Logger", d.Logger == nil},
		{"Queries", d.Queries == nil},
		{"Messages", d.Messages == nil},
		{"Sessions", d.Sessions == nil},
		{"Settings", d.Settings == nil},
		{"Accounts", d.Accounts == nil},
		{"Models", d.Models == nil},
		{"Agent", d.Agent == nil},
		{"SessionRuntime", d.SessionRuntime == nil},
		{"BotPermissions", d.BotPermissions == nil},
		{"WorkspaceTargets", d.WorkspaceTargets == nil},
		{"ToolApproval", d.ToolApproval == nil},
		{"UserInput", d.UserInput == nil},
		{"RunCredentialSecret", d.RunCredentialSecret == ""},
	}
	for _, r := range required {
		if r.missing {
			return fmt.Errorf("agent: Deps.%s is required", r.name)
		}
	}
	// The message store is the only writer of a run's history, spawned runs
	// included, and writes it one fenced step at a time.
	if _, ok := d.Messages.(messagepkg.AgentStepPersister); !ok {
		return errors.New("agent: Deps.Messages must write agent steps")
	}
	return nil
}

// New creates the service and its components. A missing required dependency is
// an error, so a half-wired service cannot exist.
func New(d Deps) (*Service, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return assemble(d, wiring{})
}
