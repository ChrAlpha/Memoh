package application

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/botenv"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/memory"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/plan"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/subagent"
	"github.com/felinics/memoh/internal/agent/title"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
	"github.com/felinics/memoh/internal/workspace"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

// stubQueries answers the reads a run's end makes with empty results; any other
// query panics, which names the test that needs a real store.
type stubQueries struct{ dbstore.Queries }

func (stubQueries) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (stubQueries) GetDiscussSession(context.Context, sqlc.GetDiscussSessionParams) (sqlc.GetDiscussSessionRow, error) {
	return sqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}

func (stubQueries) ListUserInputsByRun(context.Context, pgtype.UUID) ([]sqlc.UserInputRequest, error) {
	return nil, nil
}

type (
	stubMessages struct{ messagepkg.Service }
	stubSessions struct{ runreq.SessionService }
)

type allowAllBotPermissions struct{}

func (allowAllBotPermissions) HasBotPermission(context.Context, string, string, string) (bool, error) {
	return true, nil
}

type stubWorkspaceTargets struct{}

func (stubWorkspaceTargets) ResolveWorkspaceTarget(context.Context, string, string) (workspace.ResolvedWorkspaceTarget, error) {
	return workspace.ResolvedWorkspaceTarget{}, nil
}

// testDeps returns Deps with every required dependency filled by a stand-in.
func testDeps(t *testing.T) Deps {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{})
	t.Cleanup(func() { _ = manager.Close() })
	return Deps{
		Logger:              slog.New(slog.DiscardHandler),
		Queries:             stubQueries{},
		Messages:            stubMessages{},
		Sessions:            stubSessions{},
		Settings:            &settings.Service{},
		Accounts:            &accounts.Service{},
		Models:              &models.Service{},
		Agent:               &native.Agent{},
		SessionRuntime:      manager,
		BotPermissions:      allowAllBotPermissions{},
		WorkspaceTargets:    stubWorkspaceTargets{},
		ToolApproval:        &toolapproval.Service{},
		UserInput:           &userinput.Service{},
		RunCredentialSecret: "test-resume-secret",
	}
}

// newTestService builds a Service through the real New. Fields left zero in d
// take stand-ins for the required dependencies, so a test names only what it
// cares about.
func newTestService(t *testing.T, d Deps) *Service {
	t.Helper()
	base := testDeps(t)
	if d.Logger == nil {
		d.Logger = base.Logger
	}
	if d.Queries == nil {
		d.Queries = base.Queries
	}
	if d.Messages == nil {
		d.Messages = base.Messages
	}
	if d.Sessions == nil {
		d.Sessions = base.Sessions
	}
	if d.Settings == nil {
		d.Settings = base.Settings
	}
	if d.Accounts == nil {
		d.Accounts = base.Accounts
	}
	if d.Models == nil {
		d.Models = base.Models
	}
	if d.Agent == nil {
		d.Agent = base.Agent
	}
	if d.SessionRuntime == nil {
		d.SessionRuntime = base.SessionRuntime
	}
	if d.BotPermissions == nil {
		d.BotPermissions = base.BotPermissions
	}
	if d.WorkspaceTargets == nil {
		d.WorkspaceTargets = base.WorkspaceTargets
	}
	if d.ToolApproval == nil {
		d.ToolApproval = base.ToolApproval
	}
	if d.UserInput == nil {
		d.UserInput = base.UserInput
	}
	if d.RunCredentialSecret == "" {
		d.RunCredentialSecret = base.RunCredentialSecret
	}
	service, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// New bound the driver's events to the runtime it was given; a rewire keeps
	// them there.
	rebuildDriver(service, "events", func(dd *drive.Deps) { dd.Events = d.SessionRuntime })
	return service
}

// externalDrivers are the drivers a test registered on a Service built from a
// literal. They outlive a rewire, which rebuilds the external run host.
var externalDrivers sync.Map // *Service -> []external.Driver

// abortRuntimes are the abort acknowledgements a test replaced on a Service.
var abortRuntimes sync.Map // *Service -> externalrun.Aborter

// useAbortRuntime makes aborter acknowledge the aborts of a built service.
func useAbortRuntime(s *Service, aborter externalrun.Aborter) *Service {
	abortRuntimes.Store(s, aborter)
	s.externalrun = externalOf(s)
	rewirePlan(s)
	return s
}

// externalDriver is the driver a test registered for runtimeType.
func externalDriver(s *Service, runtimeType string) external.Driver {
	drivers, _ := externalDrivers.Load(s)
	var found external.Driver
	for _, driver := range drivers.([]external.Driver) {
		if driver.RuntimeType() == runtimeType {
			found = driver
		}
	}
	return found
}

// registerExternalDrivers adds drivers to a Service built from a literal.
func registerExternalDrivers(s *Service, drivers ...external.Driver) {
	previous, _ := externalDrivers.Load(s)
	had, _ := previous.([]external.Driver)
	externalDrivers.Store(s, append(slices.Clone(had), drivers...))
	s.externalrun = externalOf(s)
	rewirePlan(s)
}

func registerACPPool(s *Service, pool acpagent.Prompter) {
	registerExternalDrivers(s, acpagent.NewDriver(pool))
}

func TestNewRejectsMissingRequiredDependency(t *testing.T) {
	cases := map[string]func(*Deps){
		"Logger":              func(d *Deps) { d.Logger = nil },
		"Queries":             func(d *Deps) { d.Queries = nil },
		"Messages":            func(d *Deps) { d.Messages = nil },
		"Sessions":            func(d *Deps) { d.Sessions = nil },
		"Settings":            func(d *Deps) { d.Settings = nil },
		"Accounts":            func(d *Deps) { d.Accounts = nil },
		"Models":              func(d *Deps) { d.Models = nil },
		"Agent":               func(d *Deps) { d.Agent = nil },
		"SessionRuntime":      func(d *Deps) { d.SessionRuntime = nil },
		"BotPermissions":      func(d *Deps) { d.BotPermissions = nil },
		"WorkspaceTargets":    func(d *Deps) { d.WorkspaceTargets = nil },
		"ToolApproval":        func(d *Deps) { d.ToolApproval = nil },
		"UserInput":           func(d *Deps) { d.UserInput = nil },
		"RunCredentialSecret": func(d *Deps) { d.RunCredentialSecret = "" },
	}
	for name, clear := range cases {
		t.Run(name, func(t *testing.T) {
			d := testDeps(t)
			clear(&d)
			service, err := New(d)
			if err == nil || service != nil {
				t.Fatalf("New = (%v, %v), want error", service, err)
			}
			if !strings.Contains(err.Error(), "Deps."+name+" ") {
				t.Fatalf("error %q does not name %s", err, name)
			}
		})
	}
}

func TestNewAcceptsMissingOptionalDependencies(t *testing.T) {
	service, err := New(testDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.eventPublisher != nil ||
		service.queue != nil ||
		service.resumeReady != nil {
		t.Fatal("optional dependencies must stay nil when absent")
	}
}

func TestNewDefaultsTeamScopesToTheAllowedTeam(t *testing.T) {
	deps := testDeps(t)
	deps.AllowedTeam = "team-x"
	service, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := service.teamScopes.CurrentScope(context.Background()); got != "team-x" {
		t.Fatalf("current scope = %q, want the allowed team", got)
	}
	custom := runreq.SingleTeam("team-y")
	deps.TeamScopes = custom
	service, err = New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.teamScopes != custom {
		t.Fatal("provider supplied in Deps was replaced")
	}
}

func TestNewCarriesConfigurationAndRuntime(t *testing.T) {
	d := testDeps(t)
	d.AllowedTeam = "team-x"
	d.ContextFallbackTokens = 1234
	service, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.allowedTeam != "team-x" || service.transcript.AbsoluteMaxTokens() != 1234 {
		t.Fatalf("configuration not carried: team=%q budget=%d", service.allowedTeam, service.transcript.AbsoluteMaxTokens())
	}
	if service.sessionManager != d.SessionRuntime || service.decisionRuntime != d.SessionRuntime ||
		service.sessionRuntime != runreq.TurnAdmitter(d.SessionRuntime) {
		t.Fatal("session runtime must back all three runtime fields")
	}
}

// orderRecorder notes which step of the terminal fan-out ran, in order.
type orderRecorder struct {
	stubQueries
	mu    sync.Mutex
	steps []string
}

func (r *orderRecorder) note(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, step)
}

func (r *orderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

func (r *orderRecorder) GetContextLifecycleByRunID(context.Context, pgtype.UUID) (sqlc.GetContextLifecycleByRunIDRow, error) {
	r.note("lifecycle")
	return sqlc.GetContextLifecycleByRunIDRow{}, pgx.ErrNoRows
}

func (*orderRecorder) GetLatestAssistantContextLifecycleMetadataByRunID(context.Context, pgtype.UUID) ([]byte, error) {
	return nil, pgx.ErrNoRows
}

func (*orderRecorder) CreateContextLifecycle(context.Context, sqlc.CreateContextLifecycleParams) (sqlc.CreateContextLifecycleRow, error) {
	return sqlc.CreateContextLifecycleRow{}, nil
}

func (*orderRecorder) UpsertTerminalContextLifecycle(context.Context, sqlc.UpsertTerminalContextLifecycleParams) (sqlc.UpsertTerminalContextLifecycleRow, error) {
	return sqlc.UpsertTerminalContextLifecycleRow{}, nil
}

func (r *orderRecorder) GetDiscussSession(context.Context, sqlc.GetDiscussSessionParams) (sqlc.GetDiscussSessionRow, error) {
	r.note("after")
	return sqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}

type resultLogRecorder struct {
	slog.Handler
	rec *orderRecorder
}

func (h resultLogRecorder) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "agent run" {
		h.rec.note("result")
	}
	return nil
}

func (resultLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h resultLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h resultLogRecorder) WithGroup(string) slog.Handler { return h }

// A run that ends on the runtime New was given reaches the service's terminal
// observer, and its three steps run in the contract order: outcome record,
// lifecycle reconciliation, then what the end of the run releases.
func TestNewRegistersTerminalObserverInOrder(t *testing.T) {
	rec := &orderRecorder{}
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID: "new-observer-owner", OwnerLeaseTTL: time.Minute, Ledger: sessionledger.New(), Fence: abortAlignmentFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	service := newTestService(t, Deps{
		Logger:         slog.New(resultLogRecorder{rec: rec}),
		Queries:        rec,
		SessionRuntime: manager,
	})

	_, _, finish, err := service.AdmitSubagentRun(context.Background(), agenttest.LifecycleBotID, agenttest.LifecycleSessionID,
		"subagent:new-observer", []byte(`{"task":"end"}`))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	finish(tools.SubagentTerminal{OutcomeResolved: true, Outcome: tools.SpawnAttemptCompleted})

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Join(rec.snapshot(), ","); got != "result,lifecycle,after" {
		t.Fatalf("terminal steps = %s, want result,lifecycle,after", got)
	}
}

func TestTerminalFanoutRunsInOrder(t *testing.T) {
	var order []string
	step := func(name string) func(context.Context, sessionruntime.TerminalRun) {
		return func(context.Context, sessionruntime.TerminalRun) { order = append(order, name) }
	}
	terminalFanout(step("result"), step("lifecycle"), step("after"))(context.Background(), sessionruntime.TerminalRun{})
	if got := strings.Join(order, ","); got != "result,lifecycle,after" {
		t.Fatalf("order = %s", got)
	}
}

// setAllowedTeam moves a built service to another team, including the scope
// provider New derived from the previous one.
func setAllowedTeam(s *Service, teamID string) {
	s.allowedTeam = teamID
	s.teamScopes = runreq.SingleTeam(teamID)
	s.externalrun = externalOf(s)
	rewirePlan(s)
}

// useLifecycleStore routes the lifecycle rows of an already built service to
// store. The run ledger stays whatever the service reads from.
func useLifecycleStore(s *Service, store lifecycle.Store) {
	s.lifecycle = lifecycle.New(lifecycle.Deps{Store: store, Runs: s.queries, Decisions: s.decider, Logger: s.logger})
	rewireRun(s)
}

// wireComponents gives a Service built piecewise the components New would have
// built from its fields; a component the test installed stays.
func wireComponents(s *Service) *Service {
	if s.activePrompts == nil {
		s.activePrompts = activeprompt.New()
	}
	if s.botenv == nil {
		s.botenv = botenvOf(s)
	}
	if s.transcript == nil {
		s.transcript = transcriptOf(s)
	}
	if s.signer == nil {
		s.signer = admit.NewSigner("")
	}
	if s.admit == nil {
		s.admit = admitOf(s)
	}
	if s.decider == nil {
		s.decider = decideOf(s)
	}
	if s.memory == nil {
		s.memory = memoryOf(s)
	}
	if s.compact == nil {
		s.compact = compactOf(s)
	}
	if s.lifecycle == nil {
		// A Service built from a literal records no lifecycle rows, as before.
		s.lifecycle = lifecycle.New(lifecycle.Deps{Store: &agenttest.RecordingLifecycleStore{}, Runs: agenttest.NoRuns{}, Decisions: s.decider, Logger: s.logger})
	}
	if s.runconfig == nil {
		s.runconfig = runconfigOf(s)
	}
	if s.persist == nil {
		s.persist = persistOf(s)
	}
	if s.driver == nil {
		s.driver = drive.New(driveDepsOf(s))
	}
	if s.subagent == nil {
		s.subagent = subagentOf(s)
	}
	if s.externalrun == nil {
		s.externalrun = externalOf(s)
	}
	if s.title == nil {
		s.title = titleOf(s)
	}
	if s.plan == nil {
		s.plan = planOf(s)
		s.continuer = s.plan
	}
	return s
}

// rewireComponents rebuilds the components that copy the dependencies a test
// replaces after New.
func rewireComponents(s *Service) {
	s.botenv = botenvOf(s)
	s.transcript = transcriptOf(s)
	s.decider = decideOf(s)
	rebindDecisions(s)
	s.memory = memoryOf(s)
	s.compact = compactOf(s)
	rewireRun(s)
}

// rebindDecisions points the session runtime's decision callbacks at the
// service's current decider, which a rewire replaced.
func rebindDecisions(s *Service) {
	if s.decisionRuntime == nil {
		return
	}
	s.decisionRuntime.SetDecisionStore(s.decider)
	s.decisionRuntime.SetLostRunDecisionCanceller(s.decider.CancelLostRunDecisions)
	s.decisionRuntime.SetDecisionFinalizer(s.decider.FinalizeRuntimeDecisions)
}

// memoryAdjustments and compactAdjustments are the dependency overrides a test
// named for a service's memory keeper and compactor. They outlive a rewire,
// which rebuilds the components from the service's fields alone.
var (
	runconfigAdjustments sync.Map // *Service -> func(*runconfig.Deps)
	decideAdjustments    sync.Map // *Service -> func(*decide.Deps)
	memoryAdjustments    sync.Map // *Service -> func(*memory.Deps)
	compactAdjustments   sync.Map // *Service -> func(*compact.Deps)
)

// useRunconfig replaces the service's run configuration builder with one whose
// dependencies adjust has changed.
func useRunconfig(s *Service, adjust func(*runconfig.Deps)) *Service {
	runconfigAdjustments.Store(s, adjust)
	s.runconfig = runconfigOf(s)
	s.externalrun = externalOf(s)
	s.title = titleOf(s)
	rewirePlan(s)
	return s
}

// useDecisions replaces the service's decider with one whose dependencies
// adjust has changed.
func useDecisions(s *Service, adjust func(*decide.Deps)) *Service {
	decideAdjustments.Store(s, adjust)
	s.decider = decideOf(s)
	rebindDecisions(s)
	s.runconfig = runconfigOf(s)
	s.externalrun = externalOf(s)
	rewirePlan(s)
	return s
}

// useMemory replaces the service's memory keeper with one whose dependencies
// adjust has changed.
func useMemory(s *Service, adjust func(*memory.Deps)) *Service {
	memoryAdjustments.Store(s, adjust)
	s.memory = memoryOf(s)
	rewireRun(s)
	return s
}

// withMemory gives a Service built from a literal the memory keeper New builds
// for registry.
func withMemory(s *Service, registry *memprovider.Registry) *Service {
	return useMemory(s, func(d *memory.Deps) { d.Registry = registry })
}

// useCompaction replaces the service's compactor with one whose dependencies
// adjust has changed.
func useCompaction(s *Service, adjust func(*compact.Deps)) *Service {
	compactAdjustments.Store(s, adjust)
	s.compact = compactOf(s)
	rewireRun(s)
	return s
}

// rewireRun rebuilds the history writer and the driver, which copy the message
// store, the session store and the session runtime a test replaces after New.
func rewireRun(s *Service) {
	s.admit = admitOf(s)
	s.runconfig = runconfigOf(s)
	s.persist = persistOf(s)
	s.driver = drive.New(driveDepsOf(s))
	s.subagent = subagentOf(s)
	s.externalrun = externalOf(s)
	s.title = titleOf(s)
	rewirePlan(s)
}

// driverAdjustments are the dependency overrides a test named for a service's
// driver, by what they override. They outlive a rewire, which would otherwise
// rebuild the driver from the service's fields alone, and a new session runtime
// drops the ones that belonged to the old one.
var driverAdjustments sync.Map // *Service -> map[string]func(*drive.Deps)

// rebuildDriver replaces the service's driver with one whose dependencies
// mutate has adjusted: the session runtime's event intake ("events"), the
// runtime that writes terminal states ("runtime"), the output stores
// ("output"), the idle windows ("idle").
func rebuildDriver(s *Service, kind string, mutate func(*drive.Deps)) {
	previous, _ := driverAdjustments.Load(s)
	adjustments := map[string]func(*drive.Deps){}
	if had, ok := previous.(map[string]func(*drive.Deps)); ok {
		for k, v := range had {
			adjustments[k] = v
		}
	}
	adjustments[kind] = mutate
	driverAdjustments.Store(s, adjustments)
	s.admit = admitOf(s)
	s.driver = drive.New(driveDepsOf(s))
	s.subagent = subagentOf(s)
	s.externalrun = externalOf(s)
	rewirePlan(s)
}

// bindRuntime is bindSessionRuntime for a Service built from a literal: the
// history writer and the driver follow the new runtime.
func bindRuntime(s *Service, manager *sessionruntime.Manager) {
	s.bindSessionRuntime(manager)
	s.admit = admitOf(s)
	if previous, ok := driverAdjustments.Load(s); ok {
		adjustments := map[string]func(*drive.Deps){}
		for k, v := range previous.(map[string]func(*drive.Deps)) {
			if k != "events" && k != "runtime" {
				adjustments[k] = v
			}
		}
		driverAdjustments.Store(s, adjustments)
	}
	rebuildDriver(s, "events", func(d *drive.Deps) { d.Events = manager })
	s.externalrun = externalOf(s)
	rewirePlan(s)
}

func persistOf(s *Service) *persist.Writer {
	runtime := persist.SteerRuntime(standInSteerRuntime())
	if s.sessionManager != nil {
		runtime = s.sessionManager
	}
	d := persist.Deps{
		Logger: s.logger, Messages: s.messageService, Sessions: s.sessionService, Bot: s.botenv,
		Runtime: runtime, Events: s.eventPublisher, Rounds: s.memory, Commits: s.compact, Promoted: s.queue,
	}
	if d.Messages == nil {
		d.Messages = &agenttest.RecordingMessages{}
	}
	if d.Sessions == nil {
		d.Sessions = agenttest.NewChatSessions()
	}
	return persist.New(d)
}

// driveDepsOf is what the driver of a Service built from a literal is given:
// the session runtime ends runs, and the events go nowhere until the service is
// bound to a manager (bindRuntime) or a test names an intake.
func driveDepsOf(s *Service) drive.Deps {
	deps := drive.Deps{
		Logger: s.logger, Lifecycle: s.lifecycle, Persist: s.persist,
		Events: agenttest.EventFunc(func(context.Context, sessionruntime.RunHandle, native.StreamEvent) error { return nil }),
	}
	if s.sessionRuntime != nil {
		deps.Runtime = s.sessionRuntime
	}
	if adjustments, ok := driverAdjustments.Load(s); ok {
		for _, adjust := range adjustments.(map[string]func(*drive.Deps)) {
			adjust(&deps)
		}
	}
	return deps
}

func botenvOf(s *Service) *botenv.Env {
	var bridgeProvider bridge.Provider
	if s.agent != nil {
		bridgeProvider = s.agent.BridgeProvider()
	}
	logger := s.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	d := botenv.Deps{Logger: s.logger, Settings: s.settingsService, Accounts: s.accountService, Bridge: bridgeProvider}
	if s.queries != nil {
		d.Queries = s.queries
	} else {
		d.Queries = agenttest.NoBotQueries{}
	}
	if d.Settings == nil {
		d.Settings = settings.NewService(logger, agenttest.BotSettingsQueries{}, nil, nil)
	}
	if d.Accounts == nil {
		d.Accounts = accounts.NewService(logger, nil)
	}
	return botenv.New(d)
}

func transcriptOf(s *Service) *transcript.Reader {
	d := transcript.Deps{Logger: s.logger, Messages: s.messageService, Queries: s.queries, Sessions: s.sessionService}
	if d.Messages == nil {
		d.Messages = &agenttest.RecordingMessages{}
	}
	if d.Queries == nil {
		d.Queries = &agenttest.RecordingCompactionLog{}
	}
	if d.Sessions == nil {
		d.Sessions = agenttest.NewChatSessions()
	}
	return transcript.New(d)
}

func memoryOf(s *Service) *memory.Keeper {
	d := memory.Deps{Logger: s.logger, Messages: s.messageService, Bot: s.botenv, Transcript: s.transcript}
	if s.settingsService != nil {
		d.Settings = s.settingsService
	}
	if adjust, ok := memoryAdjustments.Load(s); ok {
		adjust.(func(*memory.Deps))(&d)
	}
	return memory.New(d)
}

func compactOf(s *Service) *compact.Compactor {
	d := compact.Deps{Logger: s.logger, Models: s.modelsService, Queries: s.queries, Transcript: s.transcript}
	if s.settingsService != nil {
		d.Settings = s.settingsService
	}
	if adjust, ok := compactAdjustments.Load(s); ok {
		adjust.(func(*compact.Deps))(&d)
	}
	return compact.New(d)
}

func decideOf(s *Service) *decide.Decider {
	d := decide.Deps{
		Logger: s.logger, Queries: s.queries, Sessions: s.sessionService,
		BotPermissions: s.botPermissions, ActivePrompts: s.activePrompts,
	}
	if s.decisionRuntime != nil {
		d.Events = s.decisionRuntime
	}
	if s.agent != nil {
		d.Limits = s.agent
	}
	if adjust, ok := decideAdjustments.Load(s); ok {
		adjust.(func(*decide.Deps))(&d)
	}
	// What a literal Service does not name is an explicit stand-in.
	if d.Queries == nil {
		d.Queries = &agenttest.DecisionQueries{}
	}
	if d.Sessions == nil {
		d.Sessions = agenttest.NewChatSessions()
	}
	if d.ToolApproval == nil {
		d.ToolApproval = &toolapproval.Service{}
	}
	if d.UserInput == nil {
		d.UserInput = &agenttest.UserInput{}
	}
	if d.Events == nil {
		d.Events = agenttest.EventFunc(func(context.Context, sessionruntime.RunHandle, native.StreamEvent) error { return nil })
	}
	if d.Limits == nil {
		d.Limits = &native.Agent{}
	}
	return decide.New(d)
}

func runconfigOf(s *Service) *runconfig.Builder {
	d := runconfig.Deps{
		Logger: s.logger, Credentials: providers.NewService(nil, s.queries, ""), Models: s.modelsService,
		Sessions: s.sessionService, Agent: s.agent, Bot: s.botenv, Transcript: s.transcript,
		Memory: s.memory, Compact: s.compact, Decider: s.decider, BotPermissions: s.botPermissions,
	}
	if s.queries != nil {
		d.Queries = s.queries
	}
	if adjust, ok := runconfigAdjustments.Load(s); ok {
		adjust.(func(*runconfig.Deps))(&d)
	}
	if d.Sessions == nil {
		d.Sessions = agenttest.NewChatSessions()
	}
	if d.WorkspaceTargets == nil {
		d.WorkspaceTargets = stubWorkspaceTargets{}
	}
	if d.Agent == nil {
		d.Agent = &native.Agent{}
	}
	return runconfig.New(d)
}

func subagentOf(s *Service) *subagent.Host {
	d := subagent.Deps{
		Logger: s.logger, Admit: s.admit, Driver: s.driver, Lifecycle: s.lifecycle,
		Compact: s.compact, ActiveTurns: &s.activeTurns,
	}
	if s.decisionRuntime != nil {
		d.Events = s.decisionRuntime
	}
	if persister, ok := s.messageService.(messagepkg.AgentStepPersister); ok {
		d.Steps = persister
	}
	return subagent.New(d)
}

func externalOf(s *Service) *externalrun.Host {
	d := externalrun.Deps{
		Logger: s.logger, Store: s.queries, Sessions: s.sessionService, Messages: s.messageService,
		BotPermissions: s.botPermissions, Persist: s.persist, Memory: s.memory, Runconfig: s.runconfig,
		Decider: s.decider, ActivePrompts: s.activePrompts, Driver: s.driver, Lifecycle: s.lifecycle,
		AllowedTeam: s.allowedTeam,
	}
	if s.sessionRuntime != nil {
		d.Admitter = s.sessionRuntime
	}
	if s.sessionManager != nil {
		d.Runtime = s.sessionManager
		d.Aborter = s.sessionManager
	}
	if aborter, ok := abortRuntimes.Load(s); ok {
		d.Aborter = aborter.(externalrun.Aborter)
	}
	if drivers, ok := externalDrivers.Load(s); ok {
		d.Drivers = drivers.([]external.Driver)
	}
	// What a literal Service does not name is an explicit stand-in.
	if d.Store == nil {
		d.Store = agenttest.EmptyStore{}
	}
	if d.Sessions == nil {
		d.Sessions = agenttest.NewChatSessions()
	}
	if d.Messages == nil {
		d.Messages = &agenttest.RecordingMessages{}
	}
	if d.BotPermissions == nil {
		d.BotPermissions = &agenttest.PermissionChecker{}
	}
	if d.Runtime == nil {
		d.Runtime = standInSteerRuntime()
	}
	if d.Admitter == nil {
		d.Admitter = d.Runtime
	}
	if d.Aborter == nil {
		d.Aborter = d.Runtime
	}
	return externalrun.New(d)
}

// rewirePlan rebuilds the planner, which copies the components it plans with.
// A continuer a test installed stays; one that was the planner follows it.
func rewirePlan(s *Service) {
	previous := s.plan
	s.plan = planOf(s)
	if s.continuer == nil || s.continuer == continuer(previous) {
		s.continuer = s.plan
	}
}

// useContinuer replaces what resumes an answered decision, so a test can see
// the decided call and plan the resumed run itself.
func useContinuer(s *Service, c continuer) *Service {
	s.continuer = c
	return s
}

// continueFunc is a continuer made of a function.
type continueFunc func(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error)

func (f continueFunc) Continue(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error) {
	return f(ctx, call, req, live)
}

func planOf(s *Service) *plan.Planner {
	d := plan.Deps{
		Logger: s.logger, Admit: s.admit, Bot: s.botenv, Transcript: s.transcript, Lifecycle: s.lifecycle,
		Persist: s.persist, Compact: s.compact, Runconfig: s.runconfig, External: s.externalrun, Title: s.title,
		Executions: approvalExecutions{queries: s.queries},
		Store:      agenttest.EmptyStore{},
		Agent:      &native.Agent{},
		Runtime:    standInSteerRuntime(),
	}
	if s.queries != nil {
		d.Store = s.queries
	}
	if s.agent != nil {
		d.Agent = s.agent
	}
	if s.sessionManager != nil {
		d.Runtime = s.sessionManager
	}
	return plan.New(d)
}

func titleOf(s *Service) *title.Generator {
	d := title.Deps{
		Logger: s.logger, Credentials: providers.NewService(nil, s.queries, ""), Models: s.runconfig,
		Memory: s.memory, Events: s.eventPublisher,
	}
	if s.queries != nil {
		d.Store = s.queries
	}
	if s.sessionService != nil {
		d.Sessions = s.sessionService
	}
	if s.accountService != nil {
		d.Accounts = s.accountService
	}
	// A literal Service that names no session service holds sessions that
	// already carry a title, so no test starts a title run it did not ask for.
	if d.Sessions == nil {
		d.Sessions = &agenttest.Sessions{GetFn: func(_ context.Context, sessionID string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{ID: sessionID, Title: "titled"}, nil
		}}
	}
	return title.New(d)
}

func admitOf(s *Service) *admit.Admitter {
	return admit.New(admit.Deps{Runtime: s.sessionRuntime, Queries: s.queries, Signer: s.signer})
}

// useSecret gives a Service built from a literal the signing key New is given.
func useSecret(s *Service, secret string) *Service {
	s.signer = admit.NewSigner(secret)
	s.admit = admitOf(s)
	s.subagent = subagentOf(s)
	rewirePlan(s)
	return s
}

// newModelSelectionService is a Service whose model catalog and queries are fake.
func newModelSelectionService(t *testing.T, fake *agenttest.ModelQueries) *Service {
	t.Helper()
	return wireComponents(&Service{
		modelsService: models.NewService(slog.New(slog.DiscardHandler), fake),
		queries:       fake,
	})
}

// titleModelAccountStore answers the account lookup with one fixed record.
type titleModelAccountStore struct {
	dbstore.AccountStore
	account dbstore.AccountRecord
}

func (s titleModelAccountStore) GetByUserID(context.Context, string) (dbstore.AccountRecord, error) {
	return s.account, nil
}

var (
	standInManager     *sessionruntime.Manager
	standInManagerOnce sync.Once
)

// standInSteerRuntime is the live queue of a literal Service that bound no
// session runtime: an in-memory one that nothing else uses.
func standInSteerRuntime() *sessionruntime.Manager {
	standInManagerOnce.Do(func() {
		standInManager = sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{})
	})
	return standInManager
}

// newTestRecorder is a lifecycle recorder over store whose run ledger holds no
// runs and whose runs park on no decisions.
func newTestRecorder(store lifecycle.Store) *lifecycle.Recorder {
	return lifecycle.New(lifecycle.Deps{
		Store: store, Runs: agenttest.NoRuns{}, Decisions: agenttest.NoDecisions{}, Logger: slog.New(slog.DiscardHandler),
	})
}
