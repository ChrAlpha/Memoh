package agent

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
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	acpagent "github.com/felinics/memoh/internal/agent/runtime/acp"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/title"
	tools "github.com/felinics/memoh/internal/agent/tool"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
	"github.com/felinics/memoh/internal/workspace"
)

// stubQueries answers the reads a run's end makes with empty results; any other
// query panics, which names the test that needs a real store.
type stubQueries struct{ dbstore.Queries }

func (stubQueries) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (stubQueries) GetSessionInterruptedRun(context.Context, pgtype.UUID) (sqlc.SessionRun, error) {
	return sqlc.SessionRun{}, pgx.ErrNoRows
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

// testBuild is what newTestService assembles a Service from: the Deps and the
// per-component wiring. An option may change either.
type testBuild struct {
	deps   Deps
	wiring wiring
}

// testOption changes how newTestService builds a Service.
type testOption func(*testBuild)

// withDeps changes the Deps the service is built from.
func withDeps(f func(*Deps)) testOption {
	return func(b *testBuild) { f(&b.deps) }
}

// chained runs previous and then next on the same Deps.
func chained[D any](previous, next func(*D)) func(*D) {
	if previous == nil {
		return next
	}
	if next == nil {
		return previous
	}
	return func(d *D) {
		previous(d)
		next(d)
	}
}

func withTurnAdmitter(a runreq.TurnAdmitter) testOption {
	return func(b *testBuild) { b.wiring.turnAdmitter = a }
}

func withDecide(f func(*decide.Deps)) testOption {
	return func(b *testBuild) { b.wiring.decide = chained(b.wiring.decide, f) }
}

func withLifecycle(f func(*lifecycle.Deps)) testOption {
	return func(b *testBuild) { b.wiring.lifecycle = chained(b.wiring.lifecycle, f) }
}

func withCompact(f func(*compact.Deps)) testOption {
	return func(b *testBuild) { b.wiring.compact = chained(b.wiring.compact, f) }
}

func withRunconfig(f func(*runconfig.Deps)) testOption {
	return func(b *testBuild) { b.wiring.runconfig = chained(b.wiring.runconfig, f) }
}

func withDrive(f func(*drive.Deps)) testOption {
	return func(b *testBuild) { b.wiring.drive = chained(b.wiring.drive, f) }
}

func withExternalrun(f func(*externalrun.Deps)) testOption {
	return func(b *testBuild) { b.wiring.externalrun = chained(b.wiring.externalrun, f) }
}

func withPending(f func(*pending.Deps)) testOption {
	return func(b *testBuild) { b.wiring.pending = chained(b.wiring.pending, f) }
}

func withAnswer(f func(*answer.Deps)) testOption {
	return func(b *testBuild) { b.wiring.answer = chained(b.wiring.answer, f) }
}

// withLifecycleStore writes the context lifecycle rows to store instead of
// discarding them.
func withLifecycleStore(store lifecycle.Store) testOption {
	return withLifecycle(func(d *lifecycle.Deps) { d.Store = store })
}

// withSessionManager makes manager the session runtime of the service in every
// role: it admits the runs, takes their events, ends them and answers the
// decision callbacks.
func withSessionManager(manager *sessionruntime.Manager) testOption {
	return func(b *testBuild) {
		b.deps.SessionRuntime = manager
		b.wiring.turnAdmitter = manager
		b.wiring.drive = chained(b.wiring.drive, func(d *drive.Deps) { d.Events = manager })
	}
}

// withManagerEvents makes the run events of the driver go to manager, the way a
// service bound to a real session runtime publishes them.
func withManagerEvents(manager *sessionruntime.Manager) testOption {
	return withDrive(func(d *drive.Deps) { d.Events = manager })
}

// withIdleTimeout sets how long a run's stream may stay silent before the driver
// ends it.
func withIdleTimeout(d time.Duration) testOption {
	return withDrive(func(dd *drive.Deps) { dd.StreamIdleTimeout = d })
}

// withACPPool registers the ACP driver over pool.
func withACPPool(pool acpagent.Prompter) testOption {
	return withDrivers(acpagent.NewDriver(pool))
}

// withAllowedTeam restricts the service to one team, which also becomes its
// scope provider.
func withAllowedTeam(teamID string) testOption {
	return withDeps(func(d *Deps) { d.AllowedTeam = teamID })
}

// withTeamScopes makes scopes the tenancy of the service's entry components.
func withTeamScopes(scopes runreq.TeamScopes) testOption {
	return withDeps(func(d *Deps) { d.TeamScopes = scopes })
}

// withWorkspaceReadiness makes ready the check run before work the system owes
// a session starts.
func withWorkspaceReadiness(ready func(ctx context.Context, botID, targetID string) error) testOption {
	return withDeps(func(d *Deps) { d.WorkspaceReadiness = ready })
}

// withLogger sets the logger the service writes its records to.
func withLogger(logger *slog.Logger) testOption {
	return withDeps(func(d *Deps) { d.Logger = logger })
}

// withQueue gives the service the durable queue of input, which pending reads
// and persist promotes from.
func withQueue(queue pending.Store) testOption {
	return func(b *testBuild) {
		b.wiring.pending = chained(b.wiring.pending, func(d *pending.Deps) { d.Queue = queue })
		b.wiring.persist = chained(b.wiring.persist, func(d *persist.Deps) { d.Promoted = queue })
	}
}

// withLifecycleRecorder writes the lifecycle rows to store and parks no run on
// a decision.
func withLifecycleRecorder(store lifecycle.Store) testOption {
	return withLifecycle(func(d *lifecycle.Deps) { d.Store = store; d.Decisions = agenttest.NoDecisions{} })
}

// withDrivers registers the external runtime drivers of a service.
func withDrivers(drivers ...external.Driver) testOption {
	return withDeps(func(d *Deps) { d.ExternalRuntimes = append(d.ExternalRuntimes, drivers...) })
}

// newTestService builds a Service through the same assembly as New. What d
// leaves zero takes a stand-in that answers with empty results, so a test names
// only what it cares about: the lifecycle rows are discarded, the user-input
// store is a stand-in, and an unnamed session service holds titled chats so no
// test starts a title run it did not ask for. Options adjust the Deps of one
// component.
func newTestService(t *testing.T, d Deps, opts ...testOption) *Service {
	t.Helper()
	build := testBuild{deps: d}
	for _, opt := range opts {
		opt(&build)
	}
	d = build.deps
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Queries == nil {
		d.Queries = &agenttest.StandInQueries{}
	}
	if d.Messages == nil {
		d.Messages = &agenttest.RecordingMessages{}
	}
	namedSessions := d.Sessions != nil
	if !namedSessions {
		d.Sessions = agenttest.NewChatSessions()
	}
	if d.Settings == nil {
		d.Settings = settings.NewService(d.Logger, agenttest.BotSettingsQueries{}, nil, nil)
	}
	if d.Accounts == nil {
		d.Accounts = accounts.NewService(d.Logger, nil)
	}
	if d.Models == nil {
		d.Models = &models.Service{}
	}
	if d.Agent == nil {
		d.Agent = &native.Agent{}
	}
	if d.SessionRuntime == nil {
		d.SessionRuntime = sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{})
		t.Cleanup(func() { _ = d.SessionRuntime.Close() })
	}
	if d.BotPermissions == nil {
		d.BotPermissions = &agenttest.PermissionChecker{}
	}
	if d.WorkspaceTargets == nil {
		d.WorkspaceTargets = stubWorkspaceTargets{}
	}
	if d.ToolApproval == nil {
		d.ToolApproval = &toolapproval.Service{}
	}
	namedUserInput := d.UserInput != nil
	if !namedUserInput {
		d.UserInput = &userinput.Service{}
	}
	if d.RunCredentialSecret == "" {
		d.RunCredentialSecret = "test-resume-secret"
	}
	if err := d.validate(); err != nil {
		t.Fatalf("Deps: %v", err)
	}

	// The stand-ins below come first, so an option that names the same component
	// changes what they built.
	var w wiring
	w.lifecycle = func(ld *lifecycle.Deps) {
		ld.Store = &agenttest.RecordingLifecycleStore{}
		ld.Runs = agenttest.NoRuns{}
	}
	// Run events go nowhere until a test names an intake (withDrive) or admits
	// through a session runtime of its own.
	w.drive = func(dd *drive.Deps) {
		dd.Events = agenttest.EventFunc(func(context.Context, sessionruntime.RunHandle, native.StreamEvent) error { return nil })
	}
	if !namedUserInput {
		w.decide = func(dd *decide.Deps) { dd.UserInput = &agenttest.UserInput{} }
	}
	if !namedSessions {
		w.title = func(td *title.Deps) {
			td.Sessions = &agenttest.Sessions{GetFn: func(_ context.Context, sessionID string) (sessionpkg.Thread, error) {
				return sessionpkg.Thread{ID: sessionID, Title: "titled"}, nil
			}}
		}
	}
	w.lifecycle = chained(w.lifecycle, build.wiring.lifecycle)
	w.decide = chained(w.decide, build.wiring.decide)
	w.title = chained(w.title, build.wiring.title)
	w.turnAdmitter = build.wiring.turnAdmitter
	w.compact, w.runconfig = build.wiring.compact, build.wiring.runconfig
	w.drive = chained(w.drive, build.wiring.drive)
	w.persist, w.externalrun, w.plan = build.wiring.persist, build.wiring.externalrun, build.wiring.plan
	w.pending, w.answer = build.wiring.pending, build.wiring.answer
	return assemble(d, w)
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
	if service.pending.Queueing() {
		t.Fatal("optional dependencies must stay nil when absent")
	}
}

func TestNewDefaultsTeamScopesToTheAllowedTeam(t *testing.T) {
	deps := testDeps(t)
	deps.AllowedTeam = "team-x"
	scopes := func() runreq.TeamScopes {
		var got runreq.TeamScopes
		newTestService(t, deps, withPending(func(d *pending.Deps) { got = d.TeamScopes }))
		return got
	}
	if got := scopes().CurrentScope(context.Background()); got != "team-x" {
		t.Fatalf("current scope = %q, want the allowed team", got)
	}
	custom := runreq.SingleTeam("team-y")
	deps.TeamScopes = custom
	if scopes() != custom {
		t.Fatal("provider supplied in Deps was replaced")
	}
}

func TestNewCarriesConfigurationAndRuntime(t *testing.T) {
	d := testDeps(t)
	d.AllowedTeam = "team-x"
	d.ContextFallbackTokens = 1234
	var driverRuntime drive.Runtime
	service, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.allowedTeam != "team-x" || service.transcript.AbsoluteMaxTokens() != 1234 {
		t.Fatalf("configuration not carried: team=%q budget=%d", service.allowedTeam, service.transcript.AbsoluteMaxTokens())
	}
	newTestService(t, d, withDrive(func(dd *drive.Deps) { driverRuntime = dd.Runtime }))
	if driverRuntime != drive.Runtime(d.SessionRuntime) {
		t.Fatal("the session runtime must back the driver")
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
	}, withLifecycleStore(rec), withLifecycle(func(d *lifecycle.Deps) { d.Runs = rec }))

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

// webAdmitter admits every invocation as the run webTestAdmission names for the
// bot and session of the call.
type webAdmitter struct{ agenttest.LifecycleAdmitter }

func (a *webAdmitter) Admit(ctx context.Context, in sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	a.Admission = webTestAdmission(runreq.ChatRequest{BotID: in.BotID, ThreadID: in.SessionID})
	return a.LifecycleAdmitter.Admit(ctx, in)
}

// withWebAdmitter replaces the session runtime with one that admits every Web
// send as a started run.
func withWebAdmitter() testOption { return withTurnAdmitter(&webAdmitter{}) }

// withWebAdmission replaces the session runtime with one that admits every Web
// send as the run webTestAdmission names for req.
func withWebAdmission(req runreq.ChatRequest) testOption {
	return withTurnAdmitter(&agenttest.LifecycleAdmitter{Admission: webTestAdmission(req)})
}
