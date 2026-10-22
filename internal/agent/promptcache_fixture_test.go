package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	"github.com/felinics/memoh/internal/db/dbtest"
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
		e.pool = dbtest.OpenPostgres(t, ctx)
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
	drainTurn(e.t, handle)
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

// What a provider's prefix cache can reuse between two requests is the longest
// run of the prompt that both render identically, counted from its start. The
// prompt is the tools, then the system prompt, then the messages, in that
// order for every client type the agent speaks.
//
// The comparison works on the request bodies the real clients put on the wire,
// split into those units and each normalized to a stable serialization. The
// provider tokenizes the rendered prompt, not the byte layout of our JSON, so
// object key order and whitespace cannot cost a hit and are removed.
// cache_control is removed too: Anthropic reads the marker as a breakpoint
// position, and the cache key is the content before it. Other top-level fields
// (model, limits, routing keys, stream flags) are not part of the prompt.

// promptUnit is one element of the prompt: a tool definition, the system
// prompt, or a message.
type promptUnit struct {
	// Name is where the unit sits in the request, such as "messages[3]".
	Name string
	// Role is the message role, empty for tools and a bare system string.
	Role  string
	value any
	canon []byte
}

// promptFields lists the request fields that carry the prompt, in the order a
// provider renders them.
var promptFields = []string{"tools", "system", "instructions", "messages", "input"}

// promptUnits splits a request body into the units of its prompt.
func promptUnits(body []byte) ([]promptUnit, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	var units []promptUnit
	for _, field := range promptFields {
		raw, ok := request[field]
		if !ok {
			continue
		}
		value, err := decodeNumbers(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", field, err)
		}
		if items, isList := value.([]any); isList {
			for i, item := range items {
				units = append(units, newPromptUnit(fmt.Sprintf("%s[%d]", field, i), item))
			}
			continue
		}
		units = append(units, newPromptUnit(field, value))
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("request body has none of %v", promptFields)
	}
	return units, nil
}

func decodeNumbers(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func newPromptUnit(name string, value any) promptUnit {
	value = withoutCacheControl(value)
	canon, _ := json.Marshal(value)
	unit := promptUnit{Name: name, value: value, canon: canon}
	if object, ok := value.(map[string]any); ok {
		unit.Role, _ = object["role"].(string)
	}
	return unit
}

func withoutCacheControl(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key != "cache_control" {
				out[key] = withoutCacheControl(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutCacheControl(item)
		}
		return out
	default:
		return value
	}
}

// prefixReport is how much of one request the next request can reuse.
type prefixReport struct {
	PrevBytes     int
	ReusableBytes int
	// Break is where the next request first differs from the previous one;
	// nil when the previous prompt is entirely a prefix of the next.
	Break *prefixBreak
}

// Ratio is the reusable share of the previous prompt.
func (r prefixReport) Ratio() float64 {
	if r.PrevBytes == 0 {
		return 1
	}
	return float64(r.ReusableBytes) / float64(r.PrevBytes)
}

// prefixBreak locates the first difference between two prompts.
type prefixBreak struct {
	// Part says what the differing unit is: tool definitions, the system
	// prompt (with the workspace file or section inside it), history, or the
	// turn in progress.
	Part string
	// Unit and Path name the unit and the JSON path of the differing value.
	Unit string
	Path string
	// Offset is the byte offset of the first differing byte within the value.
	Offset     int
	Prev, Next string
}

func (b prefixBreak) String() string {
	return fmt.Sprintf("%s at %s (%s, byte %d): previous %q, next %q", b.Part, b.Unit, b.Path, b.Offset, b.Prev, b.Next)
}

// comparePrompts measures how much of prev's prompt next reuses.
func comparePrompts(prev, next []byte) (prefixReport, error) {
	prevUnits, err := promptUnits(prev)
	if err != nil {
		return prefixReport{}, fmt.Errorf("previous request: %w", err)
	}
	nextUnits, err := promptUnits(next)
	if err != nil {
		return prefixReport{}, fmt.Errorf("next request: %w", err)
	}
	var report prefixReport
	for _, unit := range prevUnits {
		report.PrevBytes += len(unit.canon)
	}
	lastUser := lastUserUnit(nextUnits)
	for i, unit := range prevUnits {
		if i >= len(nextUnits) {
			report.Break = &prefixBreak{Part: "prompt shortened", Unit: unit.Name, Path: unit.Name, Prev: excerpt(string(unit.canon), 0)}
			return report, nil
		}
		other := nextUnits[i]
		if bytes.Equal(unit.canon, other.canon) {
			report.ReusableBytes += len(unit.canon)
			continue
		}
		report.ReusableBytes += commonPrefixLen(unit.canon, other.canon)
		path, prevText, nextText, offset := firstDifference(unit.Name, unit.value, other.value)
		report.Break = &prefixBreak{
			Part:   classifyBreak(other, i, lastUser, prevText, offset),
			Unit:   other.Name,
			Path:   path,
			Offset: offset,
			Prev:   excerpt(prevText, offset),
			Next:   excerpt(nextText, offset),
		}
		return report, nil
	}
	return report, nil
}

func commonPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// lastUserUnit is the index of the newest user message of a prompt, the start
// of the turn in progress; -1 when there is none.
func lastUserUnit(units []promptUnit) int {
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].Role == "user" {
			return i
		}
	}
	return -1
}

// classifyBreak names what the differing unit is. Text of the system prompt
// is attributed to the workspace file heading it sits under.
func classifyBreak(unit promptUnit, index, lastUser int, prevText string, offset int) string {
	switch {
	case strings.HasPrefix(unit.Name, "tools["):
		return "tool definitions"
	case strings.HasPrefix(unit.Name, "system") || strings.HasPrefix(unit.Name, "instructions") || unit.Role == "system" || unit.Role == "developer":
		return "system prompt: " + systemSection(prevText, offset)
	case lastUser >= 0 && index >= lastUser:
		return "current turn"
	default:
		return "history"
	}
}

// systemSection finds the heading the byte at offset sits under: a workspace
// file the prompt includes as "## <name>", or the prompt's own text.
func systemSection(text string, offset int) string {
	if offset > len(text) {
		offset = len(text)
	}
	head := text[:offset]
	at := strings.LastIndex(head, "\n## ")
	if at < 0 {
		if strings.HasPrefix(head, "## ") {
			at = -1
		} else {
			return "prompt text"
		}
	}
	line := head[at+1:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	switch strings.TrimSpace(strings.TrimPrefix(line, "## ")) {
	case "MEMORY.md":
		return "memory overview (MEMORY.md)"
	case "AGENTS.md", "PROFILES.md":
		return "workspace file (" + strings.TrimSpace(strings.TrimPrefix(line, "## ")) + ")"
	default:
		return "prompt section (" + strings.TrimSpace(line) + ")"
	}
}

// firstDifference walks two decoded values in step and reports the JSON path of
// the first leaf that differs, the two texts there, and the byte offset where
// they diverge.
func firstDifference(path string, a, b any) (string, string, string, int) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return path, describe(a), describe(b), 0
		}
		for _, key := range sortedKeys(av, bv) {
			ai, aok := av[key]
			bi, bok := bv[key]
			if !aok || !bok {
				return path + "." + key, describeMissing(ai, aok), describeMissing(bi, bok), 0
			}
			if !bytes.Equal(canonical(ai), canonical(bi)) {
				return firstDifference(path+"."+key, ai, bi)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return path, describe(a), describe(b), 0
		}
		for i := range max(len(av), len(bv)) {
			if i >= len(av) || i >= len(bv) {
				return fmt.Sprintf("%s[%d]", path, i), "", "", 0
			}
			if !bytes.Equal(canonical(av[i]), canonical(bv[i])) {
				return firstDifference(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i])
			}
		}
	case string:
		if bs, ok := b.(string); ok {
			offset := commonPrefixLen([]byte(av), []byte(bs))
			for offset > 0 && !utf8.RuneStart(av[min(offset, len(av)-1)]) {
				offset--
			}
			return path, av, bs, offset
		}
	}
	return path, describe(a), describe(b), 0
}

func canonical(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}

func describe(value any) string { return string(canonical(value)) }

func describeMissing(value any, present bool) string {
	if !present {
		return "(absent)"
	}
	return describe(value)
}

func sortedKeys(a, b map[string]any) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, m := range []map[string]any{a, b} {
		for key := range m {
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// excerpt shows the text around a byte offset, enough to see what changed.
func excerpt(text string, offset int) string {
	const around = 40
	start := max(offset-around, 0)
	end := min(offset+around, len(text))
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	out := text[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(text) {
		out += "..."
	}
	return out
}

// wireStep is one scripted model reply: a tool call, or plain text.
type wireStep struct {
	// Tool and Args make the reply a call of that tool; Args is its JSON.
	Tool string
	Args string
	// Text is the reply when Tool is empty.
	Text string
}

// wireRequest is one request body the model endpoint received, as the bytes
// the real client put on the wire.
type wireRequest struct {
	// Turn is the label the test had set when the request arrived.
	Turn string
	// Step counts the requests of that turn, from 1.
	Step int
	// Summaries is how many compaction summaries the endpoint had served when
	// the request arrived.
	Summaries int
	Body      []byte
}

// wireProvider is a model endpoint speaking the three streaming protocols the
// agent's clients use. It keeps every request body untouched and answers each
// with the next scripted step, or a short text once the script is spent.
type wireProvider struct {
	server *httptest.Server

	mu       sync.Mutex
	steps    []wireStep
	turn     string
	turnStep int
	requests []wireRequest
	calls    int
	// summaries counts the non-streamed requests, which are the compaction
	// service's summaries and not part of the session's request sequence.
	summaries int
}

func newWireProvider(t *testing.T) *wireProvider {
	t.Helper()
	w := &wireProvider{}
	w.server = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.server.Close)
	return w
}

// URL is the base URL of a provider row pointing at this endpoint.
func (w *wireProvider) URL() string { return w.server.URL }

// script queues the replies of the requests that follow.
func (w *wireProvider) script(steps ...wireStep) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.steps = append(w.steps, steps...)
}

// beginTurn labels the requests that arrive from now on.
func (w *wireProvider) beginTurn(label string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.turn, w.turnStep = label, 0
}

// recorded returns the request bodies received so far.
func (w *wireProvider) recorded() []wireRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wireRequest(nil), w.requests...)
}

func (w *wireProvider) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		http.Error(rw, "unexpected request", http.StatusBadRequest)
		return
	}
	if !probe.Stream {
		w.serveSummary(rw, r)
		return
	}
	w.mu.Lock()
	w.turnStep++
	w.calls++
	n := w.calls
	w.requests = append(w.requests, wireRequest{Turn: w.turn, Step: w.turnStep, Summaries: w.summaries, Body: body})
	step := wireStep{Text: "ok"}
	if len(w.steps) > 0 {
		step, w.steps = w.steps[0], w.steps[1:]
	}
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(http.StatusOK)
	callID := fmt.Sprintf("call_%d", n)
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		writeCompletionsStream(rw, callID, step)
	case strings.HasSuffix(r.URL.Path, "/responses"):
		writeResponsesStream(rw, callID, step)
	case strings.HasSuffix(r.URL.Path, "/messages"):
		writeAnthropicStream(rw, callID, step)
	default:
		http.Error(rw, "unknown path "+r.URL.Path, http.StatusNotFound)
		return
	}
	if flusher, ok := rw.(http.Flusher); ok {
		flusher.Flush()
	}
}

// serveSummary answers the compaction service's summary request. A run's own
// model calls are streamed, so a request that is not belongs to no turn and
// stays out of the recorded sequence.
func (w *wireProvider) serveSummary(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	w.summaries++
	text := fmt.Sprintf("SUMMARY-%d %s", w.summaries, strings.Repeat("recap ", 200))
	w.mu.Unlock()

	rw.Header().Set("Content-Type", "application/json")
	var reply map[string]any
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		reply = map[string]any{
			"id": "c", "object": "chat.completion", "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
		}
	case strings.HasSuffix(r.URL.Path, "/responses"):
		reply = map[string]any{
			"id": "resp_1", "created_at": 1700000000, "model": "m",
			"output": []map[string]any{{"type": "message", "id": "msg_1", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}}}},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	case strings.HasSuffix(r.URL.Path, "/messages"):
		reply = map[string]any{
			"id": "msg_1", "type": "message", "model": "m", "role": "assistant",
			"content":     []map[string]any{{"type": "text", "text": text}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
	default:
		http.Error(rw, "unknown path "+r.URL.Path, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(rw).Encode(reply)
}

func sseData(rw io.Writer, event, data string) {
	if event != "" {
		_, _ = fmt.Fprintf(rw, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(rw, "data: %s\n\n", data)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func writeCompletionsStream(rw io.Writer, callID string, step wireStep) {
	if step.Tool != "" {
		sseData(rw, "", fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":null}]}`, callID, step.Tool, jsonString(step.Args)))
		sseData(rw, "", `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	} else {
		sseData(rw, "", fmt.Sprintf(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":%s},"finish_reason":null}]}`, jsonString(step.Text)))
		sseData(rw, "", `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}
	sseData(rw, "", "[DONE]")
}

func writeResponsesStream(rw io.Writer, callID string, step wireStep) {
	sseData(rw, "response.created", `{"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"m"}}`)
	if step.Tool != "" {
		item := fmt.Sprintf(`{"type":"function_call","id":"fc_%s","call_id":%q,"name":%q`, callID, callID, step.Tool)
		sseData(rw, "response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":%s}}`, item))
		sseData(rw, "response.function_call_arguments.delta", fmt.Sprintf(`{"type":"response.function_call_arguments.delta","item_id":"fc_%s","output_index":0,"delta":%s}`, callID, jsonString(step.Args)))
		sseData(rw, "response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s,"arguments":%s}}`, item, jsonString(step.Args)))
	} else {
		sseData(rw, "response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`)
		sseData(rw, "response.output_text.delta", fmt.Sprintf(`{"type":"response.output_text.delta","item_id":"msg_1","delta":%s}`, jsonString(step.Text)))
		sseData(rw, "response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`)
	}
	sseData(rw, "response.completed", `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`)
}

func writeAnthropicStream(rw io.Writer, callID string, step wireStep) {
	sseData(rw, "message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","model":"m","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)
	if step.Tool != "" {
		sseData(rw, "content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, callID, step.Tool))
		sseData(rw, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}`, jsonString(step.Args)))
		sseData(rw, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sseData(rw, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
	} else {
		sseData(rw, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sseData(rw, "content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonString(step.Text)))
		sseData(rw, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sseData(rw, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
	}
	sseData(rw, "message_stop", `{"type":"message_stop"}`)
}

// promptCacheClientTypes are the client types the benchmark measures. The
// OpenAI Responses one is what dev models use.
var promptCacheClientTypes = []models.ClientType{
	models.ClientTypeOpenAIResponses,
	models.ClientTypeOpenAICompletions,
	models.ClientTypeAnthropicMessages,
}
