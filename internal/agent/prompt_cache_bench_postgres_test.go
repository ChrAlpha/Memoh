package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
	memprovider "github.com/felinics/memoh/internal/memory/adapters"
	"github.com/felinics/memoh/internal/memory/storefs"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/workspace/bridge"
	"github.com/felinics/memoh/internal/workspace/bridgepb"
	"github.com/felinics/memoh/internal/workspace/bridgesvc"
)

// These tests measure what a provider's prefix cache can reuse between
// consecutive requests of one session: the agent runs against PostgreSQL, a
// real workspace directory and the real model clients, the model being an
// endpoint that records each request body as sent. Memoh controls only whether
// a request begins with the previous one, so that is what is asserted and
// printed. The reuse table appears under -v.

// benchTool is the one tool the scripted model calls; its result is fixed.
const benchTool = "bench_lookup"

type benchToolProvider struct{}

func (benchToolProvider) Tools(context.Context, tools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{{
		Name:        benchTool,
		Description: "Looks a topic up and returns a fixed note.",
		Parameters: toolexec.SchemaFromValue(map[string]any{
			"type":       "object",
			"properties": map[string]any{"topic": map[string]any{"type": "string"}},
			"required":   []string{"topic"},
		}),
		Execute: func(_ *toolexec.ToolExecContext, input sdk.ToolArguments) (sdk.ToolOutput, error) {
			var args struct {
				Topic string `json:"topic"`
			}
			if err := input.Unmarshal(&args); err != nil {
				return sdk.ToolOutput{}, err
			}
			return toolexec.OutputFromValue(map[string]any{"note": "fixed note about " + args.Topic}), nil
		},
	}}, nil
}

// benchMemory is the bot's memory provider. After each round it writes one
// memory the way the built-in provider does, through the file store with the
// context it was handed, and reports that it has.
type benchMemory struct {
	memprovider.Provider
	files   *storefs.Service
	written chan struct{}

	mu sync.Mutex
	n  int
}

func (*benchMemory) Type() string { return "bench" }

func (*benchMemory) OnBeforeChat(context.Context, memprovider.BeforeChatRequest) (*memprovider.BeforeChatResult, error) {
	return nil, nil
}

func (m *benchMemory) OnAfterChat(ctx context.Context, req memprovider.AfterChatRequest) error {
	m.mu.Lock()
	m.n++
	n := m.n
	m.mu.Unlock()
	defer func() { m.written <- struct{}{} }()
	if err := m.files.PersistMemories(ctx, req.BotID, []storefs.MemoryItem{benchMemoryItem(n)}, nil); err != nil {
		return err
	}
	return m.files.SyncOverview(ctx, req.BotID)
}

func (m *benchMemory) OverviewStaleSince(botID string) (time.Time, bool) {
	return m.files.OverviewStaleSince(botID)
}

func (m *benchMemory) RefreshOverview(ctx context.Context, botID string) error {
	return m.files.RefreshOverview(ctx, botID)
}

func benchMemoryItem(n int) storefs.MemoryItem {
	return storefs.MemoryItem{
		ID:        fmt.Sprintf("bench-memory-%d", n),
		Memory:    fmt.Sprintf("The user mentioned fact number %d.", n),
		CreatedAt: time.Date(2026, 10, 1, 12, n, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

// benchMedia is a media store whose every asset is the same picture.
func benchMedia(t *testing.T) *agenttest.AssetLoader {
	t.Helper()
	picture := agenttest.RasterPNG(t)
	return &agenttest.AssetLoader{OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
		return io.NopCloser(bytes.NewReader(picture)), "image/png", nil
	}}
}

// benchSettings names the bench memory provider as the bot's, on top of the
// fixture's settings.
type benchSettings struct {
	dbstore.Queries
	compactionThreshold int32
}

func (q benchSettings) GetSettingsByBotID(ctx context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	row, err := q.Queries.GetSettingsByBotID(ctx, botID)
	row.MemoryProviderID = agenttest.UUID(agenttest.MemoryProviderID)
	row.CompactionThreshold = q.compactionThreshold
	return row, err
}

// benchStore adds the lifecycle and run reads the memory overview policy makes
// to the discuss fixture's PostgreSQL delegation.
type benchStore struct {
	discussStore
}

func (s benchStore) ListRecentContextLifecyclesBySession(ctx context.Context, p sqlc.ListRecentContextLifecyclesBySessionParams) ([]sqlc.ListRecentContextLifecyclesBySessionRow, error) {
	return s.pg.ListRecentContextLifecyclesBySession(ctx, p)
}

func (s benchStore) ListActiveSessionRunsByBot(ctx context.Context, botID pgtype.UUID) ([]sqlc.SessionRun, error) {
	return s.pg.ListActiveSessionRunsByBot(ctx, botID)
}

// newBenchWorkspace serves root as /data through the real workspace bridge.
func newBenchWorkspace(t *testing.T, root string) bridge.Provider {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	bridgepb.RegisterContainerServiceServer(server, bridgesvc.New(bridgesvc.Options{
		DefaultWorkDir: "/data", WorkspaceRoot: root, DataMount: "/data",
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	conn, err := grpc.NewClient("passthrough:///bench",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial workspace: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		server.Stop()
		<-done
		_ = listener.Close()
	})
	return hookBridgeProvider{client: bridge.NewClientFromConn(conn)}
}

// benchOptions are what a scenario varies.
type benchOptions struct {
	discuss bool
	// vision makes the model accept pictures, which the bot's media store
	// then serves.
	vision bool
	// contextWindow is the model's window; zero keeps a window large enough
	// that nothing is summarized.
	contextWindow int
	// compaction summarizes with the real compaction service when set.
	compaction bool
}

// benchEnv is one session of one client type, run end to end.
type benchEnv struct {
	t          *testing.T
	pool       *pgxpool.Pool
	botID      string
	sessionID  string
	clientType models.ClientType
	wire       *wireProvider
	workspace  string
	memory     *benchMemory
	svc        *Service
	discuss    discussEnv
	session    *agenttest.CompactionSession
	turns      int
}

func newBenchEnv(t *testing.T, clientType models.ClientType, opts benchOptions) *benchEnv {
	t.Helper()
	ctx := context.Background()
	e := &benchEnv{t: t, clientType: clientType, workspace: t.TempDir()}
	if opts.discuss {
		e.discuss = newDiscussEnv(t, "group")
		e.pool, e.botID, e.sessionID = e.discuss.pool, e.discuss.botID, e.discuss.sessionID
	} else {
		e.pool = agenttest.OpenPostgres(t, ctx)
		e.botID, e.sessionID = agenttest.CreateSessionFixture(t, ctx, e.pool)
		seedDirectLifecycleModel(t, ctx, e.pool)
	}
	e.wire = newWireProvider(t)

	logger := slog.New(slog.DiscardHandler)
	pg := postgresstore.NewQueriesWithPool(e.pool, sqlc.New(e.pool))
	bridgeProvider := newBenchWorkspace(t, e.workspace)

	// The bot's workspace holds an instruction file and a memory it already
	// had, with MEMORY.md written the way the management API writes it.
	e.writeWorkspaceFile("AGENTS.md", "Answer briefly and keep the user's notes in order.\n")
	files := storefs.New(logger, bridgeProvider)
	if err := files.PersistMemories(ctx, e.botID, []storefs.MemoryItem{benchMemoryItem(0)}, nil); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	e.memory = &benchMemory{files: files, written: make(chan struct{}, 256)}
	registry := memprovider.NewRegistry(logger)
	registry.Register(agenttest.MemoryProviderID, e.memory)

	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-bench-" + uuid.NewString(),
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(sqlc.New(e.pool), e.pool),
		Fence:         runtimefence.NewActivator(pg),
	})
	t.Cleanup(func() { _ = manager.Close() })

	agent := native.New(native.Deps{
		Logger:             logger,
		BridgeProvider:     bridgeProvider,
		ContextViewApplier: contextview.ProviderRunConfigApplier(logger),
	})
	providers := []tools.ToolProvider{benchToolProvider{}}
	if opts.discuss {
		providers = append(providers, tools.NewMessageProvider(nil, &discussSender{}, agenttest.MessageReactor{}, agenttest.ChannelParser{}, nil))
	}
	agent.SetToolProviders(providers)

	// A session is summarized only by the blocking compaction before a request,
	// which lands deterministically at the start of a turn: the threshold that
	// would start the background one is raised to the blocking share.
	var compactionThreshold int32
	if opts.compaction {
		compactionThreshold = int32(opts.contextWindow) //nolint:gosec // test window
	}
	mode := sessionpkg.TypeChat
	if opts.discuss {
		mode = sessionpkg.TypeDiscuss
	}
	store := func(q dbstore.Queries) dbstore.Queries {
		return benchStore{discussStore{Queries: q, pg: pg}}
	}
	options := []directLifecycleOption{
		withFixtureRuntime(manager),
		withFixtureMessages(messagepkg.NewService(logger, pg)),
		withFixtureQueries(store),
		withFixtureDeps(func(d *Deps) {
			d.Agent = agent
			d.RunCredentialSecret = discussTestSecret
			d.Memory = registry
			if opts.vision {
				d.Assets = benchMedia(t)
			}
			d.Settings = settings.NewService(logger, benchSettings{Queries: d.Queries, compactionThreshold: compactionThreshold}, nil, nil)
			d.Sessions = &agenttest.Sessions{
				GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{
						ID: id, BotID: e.botID, Type: mode, SessionMode: mode, RuntimeType: sessionpkg.RuntimeModel,
						Facts: sessionfacts.Preset(mode),
					}, nil
				},
				UpdateMetadataFn: func(_ context.Context, id string, _ map[string]any) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{ID: id}, nil
				},
			}
		}),
		withFixtureOptions(
			withLifecycleStore(pg),
		),
	}
	if opts.compaction {
		e.session = agenttest.NewCompactionSessionFor(t, ctx, e.pool, e.botID, e.sessionID)
		options = append(options, withFixtureOptions(withCompact(func(d *compact.Deps) {
			d.Runner = withoutModelRecord{e.session.Compaction}
		})))
	}
	chat := newChatTurnFixture(t, directLifecycleModelSuccess, options...)
	e.svc = chat.fixture.service

	provider := chat.fixture.queries.Provider
	provider.ClientType = string(clientType)
	provider.Config, _ = json.Marshal(map[string]any{"api_key": "test-key", "base_url": e.wire.URL()})
	chat.fixture.queries.Provider = provider
	window := opts.contextWindow
	if window == 0 {
		window = 200000
	}
	compat := []string{"tool-call"}
	if opts.vision {
		compat = append(compat, "vision")
	}
	for key, model := range chat.fixture.queries.Models {
		model.Config, _ = json.Marshal(map[string]any{"context_window": window, "compatibilities": compat})
		chat.fixture.queries.Models[key] = model
	}
	return e
}

func (e *benchEnv) writeWorkspaceFile(name, content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.workspace, name), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *benchEnv) readWorkspaceFile(name string) string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.workspace, name)) //nolint:gosec // test workspace path
	if err != nil {
		e.t.Fatal(err)
	}
	return string(raw)
}

// idleFor ages the session's finished runs, which is what the prompt cache
// of the session is measured from, as if the session had sat unused for d.
func (e *benchEnv) idleFor(d time.Duration) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE context_lifecycles SET created_at = created_at - $2::interval WHERE session_id = $1`,
		e.sessionID, fmt.Sprintf("%d seconds", int(d.Seconds()))); err != nil {
		e.t.Fatalf("age the session's runs: %v", err)
	}
}

// awaitMemoryWrite waits for the memory provider to have been handed a round.
func (e *benchEnv) awaitMemoryWrite() {
	e.t.Helper()
	select {
	case <-e.memory.written:
	case <-time.After(20 * time.Second):
		e.t.Fatal("the memory provider was never handed the round")
	}
}

// chatTurn runs one chat turn whose model answers with the scripted steps and
// returns once the run and the memory write behind it have finished.
func (e *benchEnv) chatTurn(label, query string, attachments []turn.Attachment, steps ...wireStep) {
	e.t.Helper()
	e.turns++
	e.wire.script(steps...)
	e.wire.beginTurn(label)
	handle, err := e.svc.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: "bench-team", Mode: turn.ModeChat,
		BotID: e.botID, ChatID: e.botID, ThreadID: e.sessionID,
		Query: query, UserVisibleText: query, Attachments: attachments,
		IdempotencyKey: fmt.Sprintf("bench-%d", e.turns),
	})
	if err != nil {
		e.t.Fatalf("StartTurn(%s): %v", label, err)
	}
	drainDriveHandle(e.t, handle)
	e.awaitMemoryWrite()
}

// discussBatch writes one batch of external input and has its run answer it.
func (e *benchEnv) discussBatch(label string, from discussParticipant, texts []string, steps ...wireStep) {
	e.t.Helper()
	e.turns++
	e.wire.script(steps...)
	e.wire.beginTurn(label)
	inputs := make([]passiveInput, len(texts))
	for i, text := range texts {
		inputs[i] = passiveInput{from: from, externalID: fmt.Sprintf("%s-%d", label, i), text: text}
	}
	positions := e.discuss.writeAll(e.t, inputs...)
	hw := positions[len(positions)-1]
	e.discuss.awaitAnswered(e.t, hw, func() (<-chan struct{}, error) {
		_, done, err := e.svc.pending.AdmitDiscussRun(context.Background(), e.botID, e.sessionID)
		return done, err
	})
}

// benchRow is one line of the reuse table.
type benchRow struct {
	scenario, client, kind, from, to string
	report                           prefixReport
}

var (
	benchRowsMu sync.Mutex
	benchRows   []benchRow
)

// pair is two consecutive requests of the session.
type benchPair struct {
	kind     string
	from, to wireRequest
	report   prefixReport
}

const (
	inTurn    = "within turn"
	crossTurn = "across turns"
)

// measure compares every two consecutive requests of the session.
func (e *benchEnv) measure() []benchPair {
	e.t.Helper()
	requests := e.wire.recorded()
	var out []benchPair
	for i := 1; i < len(requests); i++ {
		report, err := comparePrompts(requests[i-1].Body, requests[i].Body)
		if err != nil {
			e.t.Fatalf("compare request %d with %d: %v", i-1, i, err)
		}
		kind := crossTurn
		if requests[i-1].Turn == requests[i].Turn {
			kind = inTurn
		}
		out = append(out, benchPair{kind: kind, from: requests[i-1], to: requests[i], report: report})
	}
	return out
}

// pairs is measure, with the result kept for the reuse table.
func (e *benchEnv) pairs(scenario string) []benchPair {
	e.t.Helper()
	out := e.measure()
	benchRowsMu.Lock()
	defer benchRowsMu.Unlock()
	for _, p := range out {
		benchRows = append(benchRows, benchRow{
			scenario: scenario, client: string(e.clientType), kind: p.kind,
			from:   fmt.Sprintf("%s/%d", p.from.Turn, p.from.Step),
			to:     fmt.Sprintf("%s/%d", p.to.Turn, p.to.Step),
			report: p.report,
		})
	}
	return out
}

func (p benchPair) name() string {
	return fmt.Sprintf("%s/%d -> %s/%d", p.from.Turn, p.from.Step, p.to.Turn, p.to.Step)
}

// requireReuse fails when a request does not begin with the whole previous
// prompt. expected names the requests that may break instead, and the part
// their first difference must be in (by prefix); it returns "" for a request
// that must reuse in full. The failure names the first difference.
func requireReuse(t *testing.T, pairs []benchPair, expected func(benchPair) string) {
	t.Helper()
	if len(pairs) == 0 {
		t.Fatal("the scenario produced no request pair to measure")
	}
	var within, across int
	for _, p := range pairs {
		if p.kind == inTurn {
			within++
		} else {
			across++
		}
		if part := expected(p); part != "" {
			if p.report.Break == nil {
				t.Errorf("request %s reuses the whole previous prompt, want a break in %s", p.name(), part)
			} else if !strings.HasPrefix(p.report.Break.Part, part) {
				t.Errorf("request %s breaks in %v, want %s", p.name(), p.report.Break, part)
			}
			continue
		}
		if p.report.Break != nil {
			t.Errorf("%s request %s reuses %.1f%% of the previous prompt; first difference: %v",
				p.kind, p.name(), 100*p.report.Ratio(), p.report.Break)
		}
	}
	if within == 0 || across == 0 {
		t.Fatalf("the scenario measured %d requests within a turn and %d across turns, want both", within, across)
	}
}

// noBreak expects every request to reuse the whole previous prompt.
func noBreak(benchPair) string { return "" }

// breakInto expects the request that first enters each named turn to break, in
// the part given for that turn.
func breakInto(parts map[string]string) func(benchPair) string {
	return func(p benchPair) string {
		if p.kind != crossTurn {
			return ""
		}
		return parts[p.to.Turn]
	}
}

func toolThenAnswer(topic, answer string) []wireStep {
	return []wireStep{
		{Tool: benchTool, Args: fmt.Sprintf(`{"topic":%q}`, topic)},
		{Text: answer},
	}
}

func printBenchTable(t *testing.T) {
	t.Helper()
	benchRowsMu.Lock()
	defer benchRowsMu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-30s %-20s %-13s %-14s %7s  %s\n", "scenario", "client type", "boundary", "requests", "reused", "first difference")
	for _, row := range benchRows {
		first := "none"
		if row.report.Break != nil {
			first = row.report.Break.String()
		}
		fmt.Fprintf(&b, "%-30s %-20s %-13s %-14s %6.1f%%  %s\n",
			row.scenario, row.client, row.kind, row.from+">"+row.to, 100*row.report.Ratio(), first)
	}
	t.Log(b.String())
}
