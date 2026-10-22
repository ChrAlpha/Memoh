package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
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
	"github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/workspace"
)

func ptrTo[T any](v T) *T { return &v }

// historyRows is what survives in history: the rows written, less the ones
// removed.
func historyRows(messages *agenttest.RecordingMessages) int {
	deleted := 0
	for _, ids := range messages.Deleted {
		deleted += len(ids)
	}
	return len(messages.Persisted) - deleted
}

func assertStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s = %s\nwant %s", what, gotJSON, wantJSON)
	}
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
	service, err := assemble(d, w)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return service
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

func lifecycleTestAdmission() sessionruntime.Admission {
	return sessionruntime.Admission{
		RunID:        agenttest.LifecycleRunID,
		TurnID:       "lifecycle-turn",
		TurnPosition: 1,
		Started:      true,
		Handle: sessionruntime.RunHandle{
			RunID:        agenttest.LifecycleRunID,
			BotID:        agenttest.LifecycleBotID,
			SessionID:    agenttest.LifecycleSessionID,
			FencingToken: 1,
		},
	}
}

// drainTurn reads a handle's channels to their end and returns what they
// carried. It is bounded, so a run that never ends fails the test instead of
// hanging the suite: a hung test binary reports nothing about the tests behind
// it.
func drainTurn(t *testing.T, handle turn.RunHandle) ([]turn.Event, []error) {
	t.Helper()
	var (
		events []turn.Event
		errs   []error
	)
	deadline := time.After(10 * time.Second)
	for eventCh, errCh := handle.Events(), handle.Errs(); eventCh != nil || errCh != nil; {
		select {
		case event, ok := <-eventCh:
			if !ok {
				eventCh = nil
				continue
			}
			events = append(events, event)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			errs = append(errs, err)
		case <-deadline:
			t.Fatalf("turn did not end; events so far = %d, errors = %v", len(events), errs)
		}
	}
	return events, errs
}

// noDecisionReads answers a run's pending-decision reads as none.
type noDecisionReads struct{}

func (noDecisionReads) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]sqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (noDecisionReads) ListUserInputsByRun(context.Context, pgtype.UUID) ([]sqlc.UserInputRequest, error) {
	return nil, nil
}
