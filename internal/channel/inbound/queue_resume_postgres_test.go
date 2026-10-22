package inbound_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent"
	"github.com/felinics/memoh/internal/agent/adapter/channelbusyqueue"
	"github.com/felinics/memoh/internal/agent/agenttest"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/inbound"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/team"
	"github.com/felinics/memoh/internal/workspace"
)

// These tests restart a server whose session was running and has a platform
// message waiting behind that run, and start the message the way the channel
// process does: the busy queue sweep claims it and starts a turn through the
// turn port. The ledger, the queue and the history are PostgreSQL's, the model
// is scripted. Each case is skipped unless TEST_POSTGRES_DSN names a database.

const (
	resumeSigningKey = "queue-resume-secret"
	resumePlatform   = "telegram"
	resumeTarget     = "chat-1"
)

// resumeStep is one scripted model call: reached closes when the call arrives,
// and gate, when set, holds the answer until the test closes it.
type resumeStep struct {
	reached chan struct{}
	gate    chan struct{}
	text    string
}

func heldResumeStep(text string) *resumeStep {
	return &resumeStep{reached: make(chan struct{}), gate: make(chan struct{}), text: text}
}

type resumeModel struct {
	mu       sync.Mutex
	steps    []*resumeStep
	requests int
}

func (*resumeModel) Name() string                                    { return "queue-resume" }
func (*resumeModel) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }
func (*resumeModel) Test(context.Context) error                      { return nil }
func (*resumeModel) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (*resumeModel) DoGenerate(context.Context, sdk.Request) (sdk.ModelResult, error) {
	return sdk.ModelResult{FinishReason: sdk.FinishReasonStop}, nil
}

func (m *resumeModel) DoStream(ctx context.Context, _ sdk.Request) (<-chan sdk.StreamPart, error) {
	m.mu.Lock()
	m.requests++
	step := &resumeStep{}
	if len(m.steps) > 0 {
		step, m.steps = m.steps[0], m.steps[1:]
	}
	m.mu.Unlock()
	if step.reached != nil {
		close(step.reached)
	}
	if step.gate != nil {
		select {
		case <-step.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ch := make(chan sdk.StreamPart, 8)
	ch <- &sdk.StartPart{}
	ch <- &sdk.StartStepPart{}
	if step.text != "" {
		ch <- &sdk.TextStartPart{ID: "answer"}
		ch <- &sdk.TextDeltaPart{ID: "answer", Text: step.text}
	}
	ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}
	ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonStop}
	close(ch)
	return ch, nil
}

func (m *resumeModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

type allowAllPermissions struct{}

func (allowAllPermissions) HasBotPermission(context.Context, string, string, string) (bool, error) {
	return true, nil
}

type defaultWorkspaceTargets struct{}

func (defaultWorkspaceTargets) ResolveWorkspaceTarget(context.Context, string, string) (workspace.ResolvedWorkspaceTarget, error) {
	return workspace.ResolvedWorkspaceTarget{}, nil
}

// workspaceGate is the readiness check of a process.
type workspaceGate struct{ ready atomic.Bool }

func (g *workspaceGate) check(context.Context, string, string) error {
	if g.ready.Load() {
		return nil
	}
	return errors.New("workspace not ready")
}

// resumeEnv is one chat session reached through a platform route.
type resumeEnv struct {
	pool      *pgxpool.Pool
	queries   *postgresstore.Queries
	botID     string
	sessionID string
	ownerID   string
	queued    *sessionruntime.QueuedInputs
}

func newResumeEnv(t *testing.T) resumeEnv {
	t.Helper()
	ctx := context.Background()
	pool := agenttest.OpenPostgres(t, ctx)
	env := resumeEnv{
		pool: pool, queries: postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool)),
		botID: uuid.NewString(), sessionID: uuid.NewString(), ownerID: uuid.NewString(),
		queued: sessionruntime.NewQueuedInputs(dbsqlc.New(pool), pool),
	}
	providerID, modelID, routeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, query, args...)
		require.NoError(t, err, "seed %q", strings.Fields(query)[2])
	}
	exec(`
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, env.ownerID, "queue-resume-"+env.ownerID)
	exec(`INSERT INTO providers (id, name, client_type, enable, config) VALUES ($1, $2, 'openai-completions', true, '{"api_key":"queue-resume-key","base_url":"http://127.0.0.1:1"}')`,
		providerID, "queue-resume-provider-"+providerID)
	exec(`INSERT INTO models (id, model_id, name, provider_id, type, enable, config) VALUES ($1, 'queue-resume-model', 'queue-resume-model', $2, 'chat', true, '{"context_window":128000,"compatibilities":["tool-call"]}')`,
		modelID, providerID)
	exec(`INSERT INTO bots (id, owner_user_id, name, chat_model_id) VALUES ($1, $2, $3, $4)`, env.botID, env.ownerID, "queue-resume-"+env.botID, modelID)
	exec(`
		INSERT INTO bot_channel_routes (id, bot_id, channel_type, external_conversation_id, conversation_type, default_reply_target)
		VALUES ($1, $2, $3, $4, 'private', $4)
	`, routeID, env.botID, resumePlatform, resumeTarget)
	exec(`
		INSERT INTO bot_sessions (id, bot_id, route_id, channel_type, type, session_mode, runtime_type, runtime_metadata, reply_delivery, trigger_scope, decision_mode, audience, external_runtime_driver)
		VALUES ($1, $2, $3, $4, 'chat', 'chat', 'model', '{}'::jsonb, 'stream', 'addressed_message', 'auto_reject', 'shared', 'participants')
	`, env.sessionID, env.botID, routeID, resumePlatform)
	cleanupCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() {
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM bots WHERE id = $1", env.botID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM models WHERE id = $1", modelID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM providers WHERE id = $1", providerID)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", env.ownerID)
	})
	return env
}

// resumeServer is one server process over the shared database.
type resumeServer struct {
	service *agent.Service
	manager *sessionruntime.Manager
	model   *resumeModel
}

func (e resumeEnv) startServer(t *testing.T, ready func(context.Context, string, string) error, steps ...*resumeStep) resumeServer {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-queue-resume-" + uuid.NewString(),
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(e.pool), e.pool),
		Fence:         runtimefence.NewActivator(e.queries),
	})
	t.Cleanup(func() { _ = manager.Close() })

	model := &resumeModel{steps: steps}
	applier := contextview.ProviderRunConfigApplier(logger)
	nativeAgent := native.New(native.Deps{
		Logger: logger,
		ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) native.RunConfig {
			out := applier(ctx, cfg)
			if out.Model != nil {
				scripted := *out.Model
				scripted.Provider = model
				out.Model = &scripted
			}
			return out
		},
	})
	service, err := agent.New(agent.Deps{
		Logger:              logger,
		Queries:             e.queries,
		Messages:            messagepkg.NewService(logger, e.queries),
		Sessions:            sessionpkg.NewService(logger, e.queries, nil),
		Settings:            settings.NewService(logger, e.queries, nil, nil),
		Accounts:            &accounts.Service{},
		Models:              models.NewService(logger, e.queries),
		Agent:               nativeAgent,
		SessionRuntime:      manager,
		BotPermissions:      allowAllPermissions{},
		WorkspaceTargets:    defaultWorkspaceTargets{},
		ToolApproval:        &toolapproval.Service{},
		UserInput:           userinput.NewService(logger, e.queries),
		RunCredentialSecret: resumeSigningKey,
		AllowedTeam:         team.DefaultTeamID,
		ClockLocation:       time.UTC,
		QueuedInputs:        e.queued,
		WorkspaceReadiness:  ready,
	})
	require.NoError(t, err)
	return resumeServer{service: service, manager: manager, model: model}
}

func (e resumeEnv) command(invocation, text string) turn.StartTurnCommand {
	return turn.StartTurnCommand{
		SchemaVersion: turnwire.StartTurnVersion, TeamID: team.DefaultTeamID, Mode: turn.ModeChat,
		BotID: e.botID, ChatID: e.botID, ThreadID: e.sessionID,
		IdempotencyKey: invocation, ExternalMessageID: invocation,
		UserID: e.ownerID, Query: text, UserVisibleText: text,
		CurrentChannel: resumePlatform, ReplyTarget: resumeTarget, ConversationType: "private",
	}
}

// interrupt runs a turn on a process, stops the process the way a shutdown
// does, and returns the interrupted run.
func (e resumeEnv) interrupt(t *testing.T, task string) {
	t.Helper()
	held := heldResumeStep("never delivered")
	server := e.startServer(t, nil, held)
	handle, err := server.service.StartTurn(context.Background(), e.command("first-"+e.sessionID, task))
	require.NoError(t, err)
	go func() {
		for range handle.Events() {
		}
	}()
	<-held.reached
	// The request row a chat turn writes in the run's turn.
	_, err = e.pool.Exec(context.Background(), `
		INSERT INTO bot_history_messages (id, bot_id, session_id, role, content, run_id, turn_id, turn_position, turn_message_seq, turn_visible)
		SELECT $1, bot_id, session_id, 'user', '{}', run_id, turn_id, turn_position, 1, true
		FROM session_runs WHERE session_id = $2
	`, uuid.NewString(), e.sessionID)
	require.NoError(t, err)
	require.NoError(t, server.manager.InterruptForShutdown(context.Background()))
	require.NoError(t, server.service.DrainActiveTurns(context.Background()))
}

// parkPlatform parks a message the way the channel side does when it meets a
// busy session.
func (e resumeEnv) parkPlatform(t *testing.T, invocation, text string) {
	t.Helper()
	cmd := e.command(invocation, text)
	payload, err := turnwire.NewQueuedInput(cmd, &auth.RouteCredentials{
		BotID: e.botID, ChatID: e.botID, ReplyTarget: resumeTarget, UserID: e.ownerID,
	}, time.Now()).Marshal()
	require.NoError(t, err)
	_, err = e.queued.Enqueue(context.Background(), sessionruntime.QueueEnqueue{
		Key:    sessionruntime.Key{BotID: e.botID, SessionID: e.sessionID},
		Origin: sessionruntime.QueueOriginPlatformBusy, InvocationID: invocation, Payload: payload,
	})
	require.NoError(t, err)
}

func (e resumeEnv) queuedState(t *testing.T, invocation string) string {
	t.Helper()
	var state string
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT state FROM session_queued_inputs WHERE session_id = $1 AND invocation_id = $2`, e.sessionID, invocation).Scan(&state))
	return state
}

// ledgerRun is one run of the session as the ledger holds it.
type ledgerRun struct {
	runID, invocation, state string
	position                 int64
	created                  time.Time
	hasResumeIntent          bool
}

func (e resumeEnv) runs(t *testing.T) []ledgerRun {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT run_id::text, invocation_id, state, turn_position, created_at, input_json ? 'resume'
		FROM session_runs WHERE session_id = $1 ORDER BY turn_position, created_at
	`, e.sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var runs []ledgerRun
	for rows.Next() {
		var run ledgerRun
		require.NoError(t, rows.Scan(&run.runID, &run.invocation, &run.state, &run.position, &run.created, &run.hasResumeIntent))
		runs = append(runs, run)
	}
	require.NoError(t, rows.Err())
	return runs
}

// neverMoreRuns fails when the session has more than n runs at any moment of
// the next second, and says which they are.
func (e resumeEnv) neverMoreRuns(t *testing.T, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runs := e.runs(t); len(runs) > n {
			t.Fatalf("%s: %+v", what, runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// busyReplySender records what the channel side delivers as the reply of a
// queued message.
type busyReplySender struct {
	mu   sync.Mutex
	sent []channel.OutboundMessage
}

func (s *busyReplySender) Send(_ context.Context, msg channel.OutboundMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (*busyReplySender) Capabilities(string) channel.ChannelCapabilities {
	return channel.ChannelCapabilities{Text: true, Preview: channel.PreviewNone}
}

func (*busyReplySender) Attach(context.Context, string, channel.Attachment, *channel.ReplyRef) error {
	return nil
}

func (*busyReplySender) React(context.Context, channel.ReactRequest) error { return nil }

func (*busyReplySender) OpenPreview(context.Context, string, channel.StreamOptions) (channel.Preview, error) {
	return nil, errors.New("the channel has no preview")
}

func (s *busyReplySender) replies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, msg := range s.sent {
		out = append(out, msg.Message.PlainText())
	}
	return out
}

type busyReplySurfaces struct{ sender *busyReplySender }

func (s busyReplySurfaces) Surface(context.Context, string, channel.ChannelType) (channel.StreamReplySender, error) {
	return s.sender, nil
}

// listedBusyQueue signals each time the sweep has read which sessions have
// platform messages to start.
type listedBusyQueue struct {
	inbound.BusyQueue
	listed chan struct{}
}

func (q *listedBusyQueue) Drainable(ctx context.Context) ([]inbound.BusySession, error) {
	sessions, err := q.BusyQueue.Drainable(ctx)
	q.listed <- struct{}{}
	return sessions, err
}

// channelProcess is the channel runtime of a restarted server: the busy queue
// over the session input queue, and the processor that starts what waits in it.
type channelProcess struct {
	processor *inbound.ChannelInboundProcessor
	sender    *busyReplySender
	listed    chan struct{}
}

func (e resumeEnv) startChannel(t *testing.T, server resumeServer) channelProcess {
	t.Helper()
	sender := &busyReplySender{}
	processor := inbound.NewChannelInboundProcessor(slog.New(slog.DiscardHandler), channel.NewRegistry(), nil, nil, server.service, nil, nil, resumeSigningKey, time.Minute)
	listed := &listedBusyQueue{BusyQueue: channelbusyqueue.New(e.queued), listed: make(chan struct{}, 64)}
	processor.SetBusyQueue(listed, busyReplySurfaces{sender: sender})
	return channelProcess{processor: processor, sender: sender, listed: listed.listed}
}

// sweep is one pass of the channel process's busy queue sweep, which starts
// whatever platform message waits for a free session.
func (c channelProcess) sweep(t *testing.T) {
	t.Helper()
	sweep := inbound.NewBusyQueueSweep(c.processor)
	require.NoError(t, sweep.Start(context.Background()))
	select {
	case <-c.listed:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not look for waiting messages")
	}
	sweep.Stop()
}

// A session whose run a restart interrupted, with a platform message parked
// behind that run, starts nothing while its workspace is down. When the
// workspace is up the continuation starts first, the message is held back until
// the continuation ends, and both are answered.
func TestPlatformQueueStartsAfterTheOwedContinuationOnceTheWorkspaceIsReady(t *testing.T) {
	env := newResumeEnv(t)
	env.interrupt(t, "the long task")
	interrupted := env.runs(t)
	require.Len(t, interrupted, 1)
	require.True(t, interrupted[0].hasResumeIntent, "the interrupted run is owed its continuation")
	const parked = "telegram:route-1:m1"
	env.parkPlatform(t, parked, "typed before the restart")

	gate := &workspaceGate{}
	held := heldResumeStep("continued")
	server := env.startServer(t, gate.check, held, &resumeStep{text: "answered the platform message"})
	channelProcess := env.startChannel(t, server)

	channelProcess.sweep(t)
	env.neverMoreRuns(t, 1, "nothing starts while the workspace is down")
	require.Equal(t, "queued", env.queuedState(t, parked), "the message waits in the queue")
	require.True(t, env.runs(t)[0].hasResumeIntent, "the continuation was not retired as superseded")

	gate.ready.Store(true)
	channelProcess.sweep(t)
	<-held.reached
	channelProcess.sweep(t)
	env.neverMoreRuns(t, 2, "the message does not start while the continuation runs")
	require.Equal(t, "queued", env.queuedState(t, parked))

	close(held.gate)
	require.Eventually(t, func() bool {
		channelProcess.sweep(t)
		runs := env.runs(t)
		return len(runs) == 3 && runs[2].state == "completed"
	}, 30*time.Second, 50*time.Millisecond)
	runs := env.runs(t)
	require.Equal(t, "resume:"+interrupted[0].runID, runs[1].invocation, "the continuation is the first run after the interrupted one")
	require.Equal(t, "completed", runs[1].state)
	require.Contains(t, runs[2].invocation, "queued:", "the message starts after it")
	require.True(t, runs[2].created.After(runs[1].created))
	require.Greater(t, runs[2].position, runs[1].position)
	require.Equal(t, "consumed", env.queuedState(t, parked))
	require.Eventually(t, func() bool {
		return strings.Join(channelProcess.sender.replies(), "|") == "answered the platform message"
	}, 10*time.Second, 10*time.Millisecond, "the message is answered on its channel")
	require.Equal(t, 2, server.model.calls(), "the continuation and the message each reached the model")
}
