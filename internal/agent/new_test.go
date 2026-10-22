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
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
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

func withPending(f func(*pending.Deps)) testOption {
	return func(b *testBuild) { b.wiring.pending = chained(b.wiring.pending, f) }
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

// An unset Compaction must reach the compactor as an absent runner, not as a
// typed nil that reads as configured.
func TestNewHandsTheCompactorNoRunnerWithoutCompaction(t *testing.T) {
	cases := []struct {
		name       string
		compaction *compaction.Service
		wantRunner bool
	}{
		{"unset", nil, false},
		{"configured", &compaction.Service{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got compact.Runner
			newTestService(t, Deps{Compaction: tc.compaction}, withCompact(func(d *compact.Deps) { got = d.Runner }))
			if (got != nil) != tc.wantRunner {
				t.Fatalf("compactor runner = %#v, want present=%t", got, tc.wantRunner)
			}
		})
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
		OwnerID: "new-observer-owner", OwnerLeaseTTL: time.Minute, Ledger: sessionledger.New(), Fence: agenttest.NoopFence{},
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
