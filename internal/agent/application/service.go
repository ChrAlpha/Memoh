// Package application orchestrates agent turns, including context assembly,
// persistence, memory, compaction, approvals, and runtime invocation.
package application

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"golang.org/x/sync/singleflight"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/background"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/subagent"
	"github.com/felinics/memoh/internal/agent/transcript"
	messageevent "github.com/felinics/memoh/internal/chat/event"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/hooks"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
)

// Service orchestrates chat with the internal agent.
type Service struct {
	activeTurns          runreq.ActiveTurnTracker
	activePrompts        *activeprompt.Registry
	botenv               *botenv.Env
	transcript           *transcript.Reader
	lifecycle            *lifecycle.Recorder
	persist              *persist.Writer
	driver               *drive.Driver
	admit                *admit.Admitter
	signer               *admit.Signer
	runconfig            *runconfig.Builder
	subagent             *subagent.Host
	teamScopes           runreq.TeamScopes
	resumeReady          func(context.Context, string, string) error
	resumeStop           context.CancelFunc
	resumeDone           chan struct{}
	agent                *native.Agent
	modelsService        *models.Service
	queries              dbstore.Queries
	messageService       messagepkg.Service
	settingsService      *settings.Service
	accountService       *accounts.Service
	sessionService       runreq.SessionService
	externalDrivers      map[string]external.Driver
	eventPublisher       messageevent.Publisher
	botPermissions       runreq.BotPermissionChecker
	decider              *decide.Decider
	memory               *memory.Keeper
	compact              *compact.Compactor
	starterPromptFlights singleflight.Group
	// continueUserInputFn overrides the application resume after a user input
	// response; nil means storeUserInputResultAndContinue. Test seam.
	continueUserInputFn func(ctx context.Context, req userinput.Request, input decide.UserInputResponseInput, result sdk.ToolResultPart) (drive.RunPlan, error)
	logger              *slog.Logger
	allowedTeam         string
	sessionRuntime      runreq.TurnAdmitter
	decisionRuntime     *sessionruntime.Manager
	abortRuntime        runtimeAbortController
	sessionManager      *sessionruntime.Manager
	// queue is the durable queue of input that met a busy session. It is nil
	// where the deployment has no database, and then nothing queues.
	queue queuedInputStore
	// pendingSessions serializes the admission of what waits on one session.
	pendingSessions sessionLocks
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
	// ResumeReadiness checks workspace transport before a continuation is
	// claimed; without it resume skips the check.
	ResumeReadiness func(ctx context.Context, botID, targetID string) error
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
			return fmt.Errorf("agent application: Deps.%s is required", r.name)
		}
	}
	return nil
}

// New creates an application service backed by the native agent. A missing
// required dependency is an error, so a half-wired service cannot exist.
func New(d Deps) (*Service, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	teamScopes := d.TeamScopes
	if teamScopes == nil {
		teamScopes = runreq.SingleTeam(d.AllowedTeam)
	}

	service := &Service{
		agent:           d.Agent,
		modelsService:   d.Models,
		queries:         d.Queries,
		messageService:  d.Messages,
		settingsService: d.Settings,
		accountService:  d.Accounts,
		sessionService:  d.Sessions,
		botPermissions:  d.BotPermissions,
		eventPublisher:  d.Events,
		externalDrivers: externalDriverMap(d.ExternalRuntimes...),
		resumeReady:     d.ResumeReadiness,
		teamScopes:      teamScopes,
		allowedTeam:     d.AllowedTeam,
		logger:          d.Logger.With(slog.String("service", "agent/application")),
	}
	service.activePrompts = activeprompt.New()
	service.decider = decide.New(decide.Deps{
		Logger:         d.Logger.With(slog.String("service", "agent/decide")),
		Queries:        d.Queries,
		Sessions:       d.Sessions,
		ToolApproval:   d.ToolApproval,
		UserInput:      d.UserInput,
		BotPermissions: d.BotPermissions,
		ActivePrompts:  service.activePrompts,
		Events:         d.SessionRuntime,
		Limits:         d.Agent,
	})
	service.botenv = botenv.New(botenv.Deps{
		Logger:        d.Logger.With(slog.String("service", "agent/botenv")),
		Queries:       d.Queries,
		Settings:      d.Settings,
		Accounts:      d.Accounts,
		Bridge:        d.Agent.BridgeProvider(),
		Hooks:         d.Hooks,
		ClockLocation: d.ClockLocation,
	})
	service.transcript = transcript.New(transcript.Deps{
		Logger:         d.Logger.With(slog.String("service", "agent/transcript")),
		Messages:       d.Messages,
		Queries:        d.Queries,
		Sessions:       d.Sessions,
		FallbackTokens: d.ContextFallbackTokens,
	})
	service.lifecycle = lifecycle.New(lifecycle.Deps{
		Store:     d.Queries,
		Runs:      d.Queries,
		Decisions: service.decider,
		Logger:    d.Logger.With(slog.String("service", "agent/lifecycle")),
	})
	service.memory = memory.New(memory.Deps{
		Logger:     d.Logger.With(slog.String("service", "agent/memory")),
		Settings:   d.Settings,
		Messages:   d.Messages,
		Bot:        service.botenv,
		Transcript: service.transcript,
		Registry:   d.Memory,
	})
	// A typed nil stored in an interface would read as configured.
	var runner compact.Runner
	if d.Compaction != nil {
		runner = d.Compaction
	}
	service.compact = compact.New(compact.Deps{
		Logger:     d.Logger.With(slog.String("service", "agent/compact")),
		Settings:   d.Settings,
		Models:     d.Models,
		Queries:    d.Queries,
		Transcript: service.transcript,
		Runner:     runner,
	})
	service.runconfig = runconfig.New(runconfig.Deps{
		Logger:             d.Logger.With(slog.String("service", "agent/runconfig")),
		Queries:            d.Queries,
		Credentials:        providers.NewService(nil, d.Queries, ""),
		Models:             d.Models,
		Sessions:           d.Sessions,
		Agent:              d.Agent,
		Bot:                service.botenv,
		Transcript:         service.transcript,
		Memory:             service.memory,
		Compact:            service.compact,
		Decider:            service.decider,
		BotPermissions:     d.BotPermissions,
		WorkspaceTargets:   d.WorkspaceTargets,
		Skills:             d.Skills,
		Assets:             d.Assets,
		PlatformIdentities: d.PlatformIdentities,
		Workdirs:           d.Workdirs,
		Background:         d.Background,
	})
	if d.QueuedInputs != nil {
		service.queue = d.QueuedInputs
	}
	service.persist = persist.New(persist.Deps{
		Logger:   d.Logger.With(slog.String("service", "agent/persist")),
		Messages: d.Messages,
		Sessions: d.Sessions,
		Bot:      service.botenv,
		Runtime:  d.SessionRuntime,
		Events:   d.Events,
		Rounds:   service.memory,
		Commits:  service.compact,
		Promoted: service.queue,
	})
	service.signer = admit.NewSigner(d.RunCredentialSecret)
	service.admit = admit.New(admit.Deps{Runtime: d.SessionRuntime, Queries: d.Queries, Signer: service.signer})
	service.driver = drive.New(drive.Deps{
		Logger:       d.Logger.With(slog.String("service", "agent/drive")),
		Runtime:      d.SessionRuntime,
		Events:       d.SessionRuntime,
		Lifecycle:    service.lifecycle,
		Persist:      service.persist,
		OutputMedia:  d.OutputMedia,
		OutputSpeech: d.OutputSpeech,
	})
	// A message store that cannot write fenced steps leaves a spawned run to its
	// terminal snapshot.
	var steps messagepkg.AgentStepPersister
	if persister, ok := d.Messages.(messagepkg.AgentStepPersister); ok {
		steps = persister
	}
	service.subagent = subagent.New(subagent.Deps{
		Logger:      d.Logger.With(slog.String("service", "agent/subagent")),
		Admit:       service.admit,
		Driver:      service.driver,
		Lifecycle:   service.lifecycle,
		Compact:     service.compact,
		ActiveTurns: &service.activeTurns,
		Events:      d.SessionRuntime,
		Steps:       steps,
	})
	service.bindSessionRuntime(d.SessionRuntime)
	return service, nil
}
