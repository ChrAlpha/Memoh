package application

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/botenv"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
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
		Logger:           slog.New(slog.DiscardHandler),
		Queries:          stubQueries{},
		Messages:         stubMessages{},
		Sessions:         stubSessions{},
		Settings:         &settings.Service{},
		Accounts:         &accounts.Service{},
		Models:           &models.Service{},
		Agent:            &native.Agent{},
		SessionRuntime:   manager,
		BotPermissions:   allowAllBotPermissions{},
		WorkspaceTargets: stubWorkspaceTargets{},
		ToolApproval:     &toolapproval.Service{},
		UserInput:        &userinput.Service{},
		ResumeSecret:     "test-resume-secret",
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
	if d.ResumeSecret == "" {
		d.ResumeSecret = base.ResumeSecret
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

// registerExternalDrivers adds drivers to a Service built from a literal.
func registerExternalDrivers(s *Service, drivers ...external.Driver) {
	if s.externalDrivers == nil {
		s.externalDrivers = map[string]external.Driver{}
	}
	for k, v := range externalDriverMap(drivers...) {
		s.externalDrivers[k] = v
	}
}

func registerACPPool(s *Service, pool acpPrompter) {
	registerExternalDrivers(s, acpagent.NewDriver(pool))
}

func TestNewRejectsMissingRequiredDependency(t *testing.T) {
	cases := map[string]func(*Deps){
		"Logger":           func(d *Deps) { d.Logger = nil },
		"Queries":          func(d *Deps) { d.Queries = nil },
		"Messages":         func(d *Deps) { d.Messages = nil },
		"Sessions":         func(d *Deps) { d.Sessions = nil },
		"Settings":         func(d *Deps) { d.Settings = nil },
		"Accounts":         func(d *Deps) { d.Accounts = nil },
		"Models":           func(d *Deps) { d.Models = nil },
		"Agent":            func(d *Deps) { d.Agent = nil },
		"SessionRuntime":   func(d *Deps) { d.SessionRuntime = nil },
		"BotPermissions":   func(d *Deps) { d.BotPermissions = nil },
		"WorkspaceTargets": func(d *Deps) { d.WorkspaceTargets = nil },
		"ToolApproval":     func(d *Deps) { d.ToolApproval = nil },
		"UserInput":        func(d *Deps) { d.UserInput = nil },
		"ResumeSecret":     func(d *Deps) { d.ResumeSecret = "" },
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
	if service.memoryRegistry != nil || service.skillLoader != nil ||
		service.assetLoader != nil || service.platformIdentities != nil || service.workdirs != nil ||
		service.compactionService != nil || service.bgManager != nil || service.eventPublisher != nil ||
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
		service.abortRuntime != runtimeAbortController(d.SessionRuntime) || service.sessionRuntime != runreq.TurnAdmitter(d.SessionRuntime) {
		t.Fatal("session runtime must back all four runtime fields")
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
}

// useLifecycleStore routes the lifecycle rows of an already built service to
// store. The run ledger stays whatever the service reads from.
func useLifecycleStore(s *Service, store lifecycle.Store) {
	s.lifecycle = lifecycle.New(lifecycle.Deps{Store: store, Runs: s.queries, Decisions: s, Logger: s.logger})
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
	if s.lifecycle == nil {
		// A Service built from a literal records no lifecycle rows, as before.
		s.lifecycle = lifecycle.New(lifecycle.Deps{Runs: s.queries, Decisions: s, Logger: s.logger})
	}
	if s.persist == nil {
		s.persist = persistOf(s)
	}
	if s.driver == nil {
		s.driver = drive.New(driveDepsOf(s))
	}
	return s
}

// rewireComponents rebuilds the components that copy the dependencies a test
// replaces after New.
func rewireComponents(s *Service) {
	s.botenv = botenvOf(s)
	s.transcript = transcriptOf(s)
	rewireRun(s)
}

// rewireRun rebuilds the history writer and the driver, which copy the message
// store, the session store and the session runtime a test replaces after New.
func rewireRun(s *Service) {
	s.persist = persistOf(s)
	s.driver = drive.New(driveDepsOf(s))
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
	s.driver = drive.New(driveDepsOf(s))
}

// bindRuntime is bindSessionRuntime for a Service built from a literal: the
// history writer and the driver follow the new runtime.
func bindRuntime(s *Service, manager *sessionruntime.Manager) {
	s.bindSessionRuntime(manager)
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
}

func persistOf(s *Service) *persist.Writer {
	var runtime persist.SteerRuntime
	if s.sessionManager != nil {
		runtime = s.sessionManager
	}
	return persist.New(persist.Deps{
		Logger: s.logger, Messages: s.messageService, Sessions: s.sessionService, Bot: s.botenv,
		Runtime: runtime, Events: s.eventPublisher, Observer: persistObserver{s}, Promoted: s.queue,
	})
}

// driveDepsOf is what the driver of a Service built from a literal is given:
// the session runtime ends runs, and the events go nowhere until the service is
// bound to a manager (bindRuntime) or a test names an intake.
func driveDepsOf(s *Service) drive.Deps {
	deps := drive.Deps{Logger: s.logger, Lifecycle: s.lifecycle, Persist: s.persist}
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
	return botenv.New(botenv.Deps{Logger: s.logger, Queries: s.queries, Settings: s.settingsService, Accounts: s.accountService, Bridge: bridgeProvider})
}

func transcriptOf(s *Service) *transcript.Reader {
	return transcript.New(transcript.Deps{Logger: s.logger, Messages: s.messageService, Queries: s.queries, Sessions: s.sessionService})
}
