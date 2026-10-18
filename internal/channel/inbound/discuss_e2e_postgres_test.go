package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	turntransport "github.com/felinics/memoh/internal/agent/turn/grpctransport"
	"github.com/felinics/memoh/internal/agent/turn/turnpb"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/identities"
	"github.com/felinics/memoh/internal/channel/route"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/chat/timeline"
	"github.com/felinics/memoh/internal/contextview"
	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/messaging"
	"github.com/felinics/memoh/internal/models"
	intrpc "github.com/felinics/memoh/internal/rpc"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/team"
	"github.com/felinics/memoh/internal/testutil/routecredentials"
	"github.com/felinics/memoh/internal/workspace"
)

// These tests carry a group conversation through the whole discuss path over
// real PostgreSQL: the inbound processor records each message, a waker tells
// the server, the server admits a run that answers through the send tool, and
// the run's end moves the cover position on. The model is scripted, the
// channel's sender records what it was asked to deliver, and everything in
// between is production code. Each case is skipped unless TEST_POSTGRES_DSN
// names a database.

const (
	e2ePlatform   = "telegram"
	e2eTarget     = "group-1"
	e2eSigningKey = "discuss-e2e-secret"
	e2eBotName    = "memohbot"
)

// e2eStep is one scripted model call.
type e2eStep struct {
	// send answers with a send tool call carrying this text; text answers with
	// plain text. Both empty is a call that stays silent.
	send string
	text string
	// reasoning, when set, is thought that comes before a send answer, in the
	// dialect DeepSeek's thinking mode replays.
	reasoning string
	// reached closes when the call arrives, and gate, when set, holds the
	// answer until the test closes it.
	reached chan struct{}
	gate    chan struct{}
}

// e2eModel answers each model call with the next scripted step, and with
// silence once the script is spent.
type e2eModel struct {
	mu       sync.Mutex
	steps    []*e2eStep
	requests []sdk.Request
}

func (*e2eModel) Name() string                                    { return "discuss-e2e" }
func (*e2eModel) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }
func (*e2eModel) Test(context.Context) error                      { return nil }
func (*e2eModel) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (*e2eModel) DoGenerate(context.Context, sdk.Request) (sdk.ModelResult, error) {
	return sdk.ModelResult{FinishReason: sdk.FinishReasonStop}, nil
}

func (m *e2eModel) DoStream(ctx context.Context, req sdk.Request) (<-chan sdk.StreamPart, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	step := &e2eStep{}
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
	ch := make(chan sdk.StreamPart, 16)
	ch <- &sdk.StartPart{}
	ch <- &sdk.StartStepPart{}
	switch {
	case step.send != "":
		if step.reasoning != "" {
			ch <- &sdk.ReasoningStartPart{ID: "think", Model: "deepseek-reasoner", Format: sdk.ReasoningFormatOpenAIChat}
			ch <- &sdk.ReasoningDeltaPart{ID: "think", Model: "deepseek-reasoner", Format: sdk.ReasoningFormatOpenAIChat, Text: step.reasoning}
			ch <- &sdk.ReasoningEndPart{ID: "think", Model: "deepseek-reasoner", Format: sdk.ReasoningFormatOpenAIChat}
		}
		ch <- &sdk.StreamToolCallPart{
			ToolCallID: "call-send-" + uuid.NewString(),
			ToolName:   tools.ToolSend().String(),
			Input:      toolexec.ArgumentsFromValue(map[string]any{"text": step.send}),
		}
		ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonToolCalls}
		ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonToolCalls}
	default:
		if step.text != "" {
			ch <- &sdk.TextStartPart{ID: "answer"}
			ch <- &sdk.TextDeltaPart{ID: "answer", Text: step.text}
		}
		ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}
		ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonStop}
	}
	close(ch)
	return ch, nil
}

func (m *e2eModel) calls() []sdk.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sdk.Request(nil), m.requests...)
}

// e2eSender records what the send tool delivered.
type e2eSender struct {
	mu   sync.Mutex
	sent []string
}

func (s *e2eSender) Send(_ context.Context, _ string, platform messaging.Platform, req messaging.SendRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, string(platform)+"/"+req.Target+": "+req.Message.Text)
	return nil
}

func (s *e2eSender) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

type e2eReactor struct{}

func (e2eReactor) React(context.Context, string, messaging.Platform, messaging.ReactRequest) error {
	return nil
}

type e2eChannelResolver struct{}

func (e2eChannelResolver) ParseChannelType(raw string) (messaging.Platform, error) {
	return messaging.Platform(raw), nil
}

// e2eSessions answers the processor's session lookups from the database, the
// way the composition root's adapter does: the facts it reads are the
// session's recorded ones.
type e2eSessions struct {
	sessions  *sessionpkg.Service
	sessionID string
}

func (s e2eSessions) get(ctx context.Context) (SessionResult, error) {
	sess, err := s.sessions.Get(ctx, s.sessionID)
	if err != nil {
		return SessionResult{}, err
	}
	return SessionResult{
		ID: sess.ID, Type: sess.Type, Mode: sess.SessionMode, Runtime: sess.RuntimeType,
		RuntimeOwnerAccountID: fmt.Sprint(sess.RuntimeMetadata["runtime_owner_account_id"]),
		ExternalRuntimeDriver: sess.Facts.ExternalRuntimeDriver, TriggerScope: sess.Facts.TriggerScope,
	}, nil
}

func (s e2eSessions) EnsureActiveSession(ctx context.Context, _, _, _ string) (SessionResult, error) {
	return s.get(ctx)
}

func (s e2eSessions) GetActiveSession(ctx context.Context, _ string) (SessionResult, error) {
	return s.get(ctx)
}

func (e2eSessions) CreateNewSession(context.Context, string, string, string, NewSessionSpec) (SessionResult, error) {
	return SessionResult{}, errors.New("the discuss e2e session is created by the fixture")
}

// e2ePermissions lets everyone use the bot: what the tests are about is how the
// input is routed, not who may speak.
type e2ePermissions struct{}

// e2eDenyPermissions is what a run sees when no external runtime is under test:
// nothing in those tests asks for a bot permission.
type e2eDenyPermissions struct{}

func (e2eDenyPermissions) HasBotPermission(context.Context, string, string, string) (bool, error) {
	return false, nil
}

type e2eWorkspaceTargets struct{}

func (e2eWorkspaceTargets) ResolveWorkspaceTarget(context.Context, string, string) (workspace.ResolvedWorkspaceTarget, error) {
	return workspace.ResolvedWorkspaceTarget{}, nil
}

func (e2ePermissions) HasBotPermission(context.Context, string, string, string) (bool, error) {
	return true, nil
}

// e2eCodex is an External Agent runtime that offers one turn command and
// answers every prompt it is given.
type e2eCodex struct {
	mu      sync.Mutex
	prompts []external.PromptInput
}

func (*e2eCodex) RuntimeType() string { return "codex" }

func (c *e2eCodex) Prompt(_ context.Context, input external.PromptInput) (external.PromptResult, error) {
	c.mu.Lock()
	c.prompts = append(c.prompts, input)
	c.mu.Unlock()
	return external.PromptResult{
		Output:        []sdk.Message{sdk.AssistantMessage("deployed")},
		Text:          "deployed",
		TurnCompleted: true,
	}, nil
}

func (*e2eCodex) Commands(context.Context, external.PromptInput) ([]external.Command, error) {
	return []external.Command{{Name: "deploy", Description: "deploy the release", Kind: turn.RuntimeCommandTurn}}, nil
}

func (*e2eCodex) ReadCommand(context.Context, external.PromptInput) (external.CommandResult, error) {
	return external.CommandResult{}, external.ErrControlUnsupported
}

func (c *e2eCodex) prompted() []external.PromptInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]external.PromptInput(nil), c.prompts...)
}

// e2eDeployment is where the channel runtime runs relative to the server that
// admits discuss runs.
type e2eDeployment string

const (
	// e2eEmbedded runs the channel runtime in the server process: the waker
	// calls the admission directly.
	e2eEmbedded e2eDeployment = "embedded"
	// e2eSplit runs it in a process of its own: the waker, and every turn,
	// reach the server over the turn RPC.
	e2eSplit e2eDeployment = "split"
	// e2eWithoutWake is the embedded deployment with the wake taken away, for
	// what only the sweep answers.
	e2eWithoutWake e2eDeployment = "without wake"
)

// recordingAdmitter keeps what the waker asked the server and what it answered.
type recordingAdmitter struct {
	inner    turn.DiscussAdmitter
	mu       sync.Mutex
	outcomes []turn.DiscussOutcome
	errs     []error
}

func (r *recordingAdmitter) AdmitDiscuss(ctx context.Context, cmd turn.DiscussAdmitCommand) (turn.DiscussOutcome, error) {
	outcome, err := r.inner.AdmitDiscuss(ctx, cmd)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.errs = append(r.errs, err)
		return outcome, err
	}
	r.outcomes = append(r.outcomes, outcome)
	return outcome, nil
}

func (r *recordingAdmitter) count(outcome turn.DiscussOutcome) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, got := range r.outcomes {
		if got == outcome {
			n++
		}
	}
	return n
}

func (r *recordingAdmitter) failures() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errs...)
}

// discussE2E is one discuss session in a group conversation, reached through a
// channel route, with a bot owner and a server that admits its runs.
type discussE2E struct {
	t         *testing.T
	pool      *pgxpool.Pool
	botID     string
	sessionID string
	routeID   string
	ownerID   string
	suffix    string

	service    *agent.Service
	manager    *sessionruntime.Manager
	model      *e2eModel
	sender     *e2eSender
	processor  *ChannelInboundProcessor
	identities *identities.Service
	cfg        channel.ChannelConfig
	sessions   *sessionpkg.Service
	queries    *postgresstore.Queries
	logger     *slog.Logger
	// admitter records every wake the processor's waker made and what the
	// server answered.
	admitter *recordingAdmitter
	// codex is the External Agent runtime the session drives, when it drives
	// one.
	codex *e2eCodex
	// registry is what the processor knows of the platform, when a test needs
	// more than the bare platform name.
	registry *channel.Registry
}

func openDiscussE2EPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("MEMOH_TEST_POSTGRES_REQUIRED") == "1" {
			t.Fatal("discuss e2e tests are required, but TEST_POSTGRES_DSN is not set")
		}
		t.Skip("skip discuss e2e test: TEST_POSTGRES_DSN is not set")
	}
	pool, err := dbpkg.OpenPostgresDSN(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open postgres pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newDiscussE2E(t *testing.T, deployment e2eDeployment, steps ...*e2eStep) *discussE2E {
	t.Helper()
	return newDiscussE2EWith(t, deployment, nil, steps...)
}

// newDiscussE2EWith is newDiscussE2E for a session that drives an External
// Agent runtime when codex is given.
func newDiscussE2EWith(t *testing.T, deployment e2eDeployment, codex *e2eCodex, steps ...*e2eStep) *discussE2E {
	t.Helper()
	ctx := context.Background()
	e := &discussE2E{
		t: t, pool: openDiscussE2EPostgres(t), botID: uuid.NewString(), sessionID: uuid.NewString(),
		routeID: uuid.NewString(), ownerID: uuid.NewString(),
		model: &e2eModel{steps: steps}, sender: &e2eSender{},
		logger: slog.New(slog.DiscardHandler), codex: codex,
	}
	e.suffix = e.botID[:8]
	e.seed(ctx)

	e.queries = postgresstore.NewQueriesWithPool(e.pool, dbsqlc.New(e.pool))
	e.manager = sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-discuss-e2e-" + uuid.NewString(),
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(e.pool), e.pool),
		Fence:         runtimefence.NewActivator(e.queries),
	})
	t.Cleanup(func() { _ = e.manager.Close() })

	applier := contextview.ProviderRunConfigApplier(e.logger)
	nativeAgent := native.New(native.Deps{
		Logger: e.logger,
		ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) native.RunConfig {
			out := applier(ctx, cfg)
			if out.Model != nil {
				model := *out.Model
				model.Provider = e.model
				out.Model = &model
			}
			return out
		},
	})
	nativeAgent.SetToolProviders([]tools.ToolProvider{
		tools.NewMessageProvider(nil, e.sender, e2eReactor{}, e2eChannelResolver{}, nil),
	})
	e.sessions = sessionpkg.NewService(e.logger, e.queries, nil)
	deps := agent.Deps{
		Logger:              e.logger,
		Queries:             e.queries,
		Messages:            messagepkg.NewService(e.logger, e.queries),
		Sessions:            e.sessions,
		Settings:            settings.NewService(e.logger, e.queries, nil, nil),
		Accounts:            &accounts.Service{},
		Models:              models.NewService(e.logger, e.queries),
		Agent:               nativeAgent,
		SessionRuntime:      e.manager,
		BotPermissions:      e2eDenyPermissions{},
		WorkspaceTargets:    e2eWorkspaceTargets{},
		ToolApproval:        &toolapproval.Service{},
		UserInput:           userinput.NewService(e.logger, e.queries),
		RunCredentialSecret: e2eSigningKey,
		AllowedTeam:         team.DefaultTeamID,
		ClockLocation:       time.UTC,
	}
	if e.codex != nil {
		deps.ExternalRuntimes = []external.Driver{e.codex}
		deps.BotPermissions = e2ePermissions{}
	}
	service, err := agent.New(deps)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	e.service = service

	e.identities = identities.NewService(e.logger, e.queries)
	e.cfg = channel.ChannelConfig{TeamID: team.DefaultTeamID, ID: "cfg-1", BotID: e.botID, ChannelType: channel.ChannelType(e2ePlatform)}
	e.connect(deployment)
	return e
}

// connect builds the channel runtime's inbound processor for a deployment.
func (e *discussE2E) connect(deployment e2eDeployment) {
	e.t.Helper()
	var turnSvc turn.Service = e.service
	var admitter turn.DiscussAdmitter = e.service
	if deployment == e2eSplit {
		client := e.dialServer()
		turnSvc, admitter = client, client
	}
	e.admitter = &recordingAdmitter{inner: admitter}
	e.processor = e.newProcessor(turnSvc)
	if deployment != e2eWithoutWake {
		e.processor.SetDiscussWaker(NewDiscussWaker(e.admitter, e.logger))
	}
}

// dialServer serves the application over the turn RPC on a loopback port, as
// the server process does, and returns the channel process's client for it.
func (e *discussE2E) dialServer() *turntransport.Client {
	e.t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatalf("listen: %v", err)
	}
	server := intrpc.NewServer(e.logger, e2eSigningKey)
	turnpb.RegisterTurnServiceServer(server, turntransport.NewServer(e.logger, e.service))
	go func() { _ = server.Serve(listener) }()
	conn, err := intrpc.Dial(listener.Addr().String(), e2eSigningKey)
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	e.t.Cleanup(func() { _ = conn.Close(); server.Stop() })
	return turntransport.NewClient(conn, turntransport.WithClientLogger(e.logger))
}

// newProcessor is the inbound processor of one channel runtime, starting its
// turns through turnSvc.
func (e *discussE2E) newProcessor(turnSvc turn.Service) *ChannelInboundProcessor {
	var routes RouteResolver = &fakeChatService{resolveResult: route.ResolveConversationResult{BotID: e.botID, RouteID: e.routeID}}
	if e.codex != nil {
		// A runtime command is found by looking the route up first.
		routes = route.NewService(e.logger, e.queries)
	}
	processor := NewChannelInboundProcessor(e.logger, e.registry, routes, messagepkg.NewService(e.logger, e.queries), turnSvc, e.identities,
		&fakePolicyService{ownerUserID: e.ownerID}, e2eSigningKey, 0)
	if e.codex != nil {
		processor.SetBotPermissionChecker(e2ePermissions{})
	}
	processor.SetSessionEnsurer(e2eSessions{sessions: e.sessions, sessionID: e.sessionID})
	processor.SetPipeline(timeline.NewEventStore(e.logger, e.queries), messagepkg.NewService(e.logger, e.queries))
	return processor
}

// seed writes the owner, the bot with its model, the route and the discuss
// session, and removes them when the test ends.
func (e *discussE2E) seed(ctx context.Context) {
	e.t.Helper()
	providerID, modelID := uuid.NewString(), uuid.NewString()
	exec := func(query string, args ...any) {
		e.t.Helper()
		if _, err := e.pool.Exec(ctx, query, args...); err != nil {
			e.t.Fatalf("seed %q: %v", strings.Fields(query)[2], err)
		}
	}
	exec(`
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, e.ownerID, "discuss-e2e-"+e.ownerID)
	exec(`INSERT INTO providers (id, name, client_type, enable, config) VALUES ($1, $2, 'openai-completions', true, '{"api_key":"discuss-e2e-key","base_url":"http://127.0.0.1:1"}')`,
		providerID, "discuss-e2e-provider-"+providerID)
	exec(`INSERT INTO models (id, model_id, name, provider_id, type, enable, config) VALUES ($1, 'discuss-e2e-model', 'discuss-e2e-model', $2, 'chat', true, '{"context_window":128000,"compatibilities":["tool-call"]}')`,
		modelID, providerID)
	exec(`INSERT INTO bots (id, owner_user_id, name, chat_model_id) VALUES ($1, $2, $3, $4)`, e.botID, e.ownerID, "discuss-e2e-"+e.botID, modelID)
	exec(`
		INSERT INTO bot_channel_routes (id, bot_id, channel_type, external_conversation_id, conversation_type, default_reply_target)
		VALUES ($1, $2, $3, $4, 'group', $4)
	`, e.routeID, e.botID, e2ePlatform, e2eTarget)
	runtime, runtimeMetadata := "model", `{}`
	if e.codex != nil {
		runtime, runtimeMetadata = "codex", fmt.Sprintf(`{"runtime_owner_account_id":%q}`, e.ownerID)
	}
	exec(`
		INSERT INTO bot_sessions (id, bot_id, route_id, channel_type, type, session_mode, runtime_type, runtime_metadata, reply_delivery, trigger_scope, decision_mode, audience, external_runtime_driver)
		VALUES ($1, $2, $3, $4, 'discuss', 'discuss', $5, $6::jsonb, 'explicit', 'uncovered_events', 'auto_reject', 'shared', 'participants')
	`, e.sessionID, e.botID, e.routeID, e2ePlatform, runtime, runtimeMetadata)
	cleanupCtx := context.WithoutCancel(ctx)
	e.t.Cleanup(func() {
		_, _ = e.pool.Exec(cleanupCtx, "DELETE FROM bots WHERE id = $1", e.botID)
		_, _ = e.pool.Exec(cleanupCtx, "DELETE FROM models WHERE id = $1", modelID)
		_, _ = e.pool.Exec(cleanupCtx, "DELETE FROM providers WHERE id = $1", providerID)
		_, _ = e.pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", e.ownerID)
	})
}

// bind links a group member's channel identity to a new account, as a member
// who signed in to the product is.
func (e *discussE2E) bind(member string) string {
	e.t.Helper()
	ctx := context.Background()
	identity, err := e.identities.UpsertChannelIdentity(ctx, e2ePlatform, e.subject(member), member, nil)
	if err != nil {
		e.t.Fatalf("channel identity of %s: %v", member, err)
	}
	userID := uuid.NewString()
	if _, err := e.pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'member' FROM created_user
	`, userID, "discuss-e2e-"+userID); err != nil {
		e.t.Fatalf("create account of %s: %v", member, err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO user_channel_identity_bindings (user_id, channel_identity_id) VALUES ($1, $2)`, userID, identity.ID); err != nil {
		e.t.Fatalf("bind %s: %v", member, err)
	}
	e.t.Cleanup(func() { _, _ = e.pool.Exec(context.WithoutCancel(ctx), "DELETE FROM users WHERE id = $1", userID) })
	return userID
}

// subject names a group member on the platform. Members are per test, so the
// tests share nothing but the database.
func (e *discussE2E) subject(member string) string { return member + "-" + e.suffix }

func (e *discussE2E) inboundMessage(member, messageID, text string, mentionsBot bool) channel.InboundMessage {
	msg := channel.InboundMessage{
		BotID:        e.botID,
		Channel:      channel.ChannelType(e2ePlatform),
		Message:      channel.Message{ID: messageID, Text: text},
		ReplyTarget:  e2eTarget,
		Sender:       channel.Identity{SubjectID: e.subject(member), DisplayName: member},
		Conversation: channel.Conversation{ID: e2eTarget, Type: channel.ConversationTypeGroup, Name: "Release crew"},
	}
	msg.Metadata = map[string]any{"bot_username": e2eBotName}
	if mentionsBot {
		msg.Metadata["is_mentioned"] = true
	}
	return msg
}

// say delivers one group message to the inbound processor.
func (e *discussE2E) say(member, messageID, text string, mentionsBot bool) error {
	e.t.Helper()
	return e.processor.HandleInbound(context.Background(), e.cfg, e.inboundMessage(member, messageID, text, mentionsBot), &fakeReplySender{})
}

func (e *discussE2E) mustSay(member, messageID, text string, mentionsBot bool) {
	e.t.Helper()
	if err := e.say(member, messageID, text, mentionsBot); err != nil {
		e.t.Fatalf("HandleInbound(%s) error = %v", messageID, err)
	}
}

// cover is the history position the session's completed discuss runs covered.
func (e *discussE2E) cover() int64 {
	e.t.Helper()
	cover, err := dbsqlc.New(e.pool).GetDiscussCoverPosition(context.Background(), dbsqlc.GetDiscussCoverPositionParams{
		SessionID: dbpkg.ParseUUIDOrEmpty(e.sessionID), States: []string{"completed"},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return cover
}

// position is the history position of the newest external input.
func (e *discussE2E) position() int64 {
	e.t.Helper()
	var position int64
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(max(turn_position), 0) FROM bot_history_messages WHERE session_id = $1 AND event_id IS NOT NULL AND role = 'user'
	`, e.sessionID).Scan(&position); err != nil {
		e.t.Fatal(err)
	}
	return position
}

func (e *discussE2E) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, append([]any{e.sessionID}, args...)...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// runs counts the session's runs by the state they ended in.
func (e *discussE2E) runs() map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT state, count(*) FROM session_runs WHERE session_id = $1 GROUP BY state`, e.sessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			e.t.Fatal(err)
		}
		out[state] = n
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// identityID is the channel identity the processor resolved a member to.
func (e *discussE2E) identityID(member string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		SELECT id::text FROM channel_identities WHERE channel_type = $1 AND channel_subject_id = $2
	`, e2ePlatform, e.subject(member)).Scan(&id); err != nil {
		e.t.Fatalf("channel identity of %s: %v", member, err)
	}
	return id
}

// credentialsOfRun are the identity claims a run was admitted with, as it
// recorded them for a resume.
func (e *discussE2E) credentialsOfRun(hw int64) (chat, session map[string]string) {
	e.t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `
		SELECT input_json->'resume' FROM session_runs WHERE session_id = $1 AND invocation_id = $2
	`, e.sessionID, "discuss:"+e.sessionID+":"+strconv.FormatInt(hw, 10)).Scan(&raw); err != nil {
		e.t.Fatalf("resume context of the run for position %d: %v", hw, err)
	}
	var resume struct {
		ChatClaims  map[string]string `json:"chat_claims"`
		TokenClaims map[string]string `json:"token_claims"`
	}
	if err := json.Unmarshal(raw, &resume); err != nil {
		e.t.Fatal(err)
	}
	return resume.ChatClaims, resume.TokenClaims
}

// eventually polls until check holds, for what the server does after the
// inbound call has returned.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// settled gives a wake that was not supposed to admit anything the time it
// would take to.
func settled() { time.Sleep(500 * time.Millisecond) }

// A group conversation runs the whole path, in either deployment: messages
// from several members are recorded, the first is answered through send, the
// two that arrive while that run holds the session wait for its end, and the
// run's end admits one run for both.
func TestDiscussE2EGroupConversation(t *testing.T) {
	for _, deployment := range []e2eDeployment{e2eEmbedded, e2eSplit} {
		t.Run(string(deployment), func(t *testing.T) {
			first := &e2eStep{reached: make(chan struct{}), gate: make(chan struct{}), send: "Alice, the freeze is Friday."}
			second := &e2eStep{send: "Bob owns the release, noted."}
			// A run calls the model again once its send has returned, and
			// stays silent on that call.
			e := newDiscussE2E(t, deployment, first, &e2eStep{}, second, &e2eStep{})

			// The first round has no run before it.
			if runs := e.runs(); len(runs) != 0 || e.cover() != 0 {
				t.Fatalf("runs = %v, cover = %d before any input, want none", runs, e.cover())
			}
			e.mustSay("alice", "m1", "is the freeze on this week?", false)
			waitClosed(t, first.reached, "the first run to call the model")

			e.mustSay("bob", "m2", "@bot who owns the release?", true)
			e.mustSay("carol", "m3", "I do, since Tuesday", false)
			hw := e.position()
			eventually(t, "both wakes to find the session held", func() bool { return e.admitter.count(turn.DiscussBusy) == 2 })
			if got := e.runs(); got["running"] != 1 || len(got) != 1 {
				t.Fatalf("runs while the first holds the session = %v, want the one", got)
			}

			close(first.gate)
			eventually(t, "the run's end to answer the second batch", func() bool { return e.cover() == hw })

			if got := e.runs(); got["completed"] != 2 || len(got) != 1 {
				t.Fatalf("runs = %v, want two completed", got)
			}
			// Only the first wake admitted anything; the second batch was
			// admitted by the end of the run holding the session.
			if admitted := e.admitter.count(turn.DiscussAdmitted); admitted != 1 {
				t.Fatalf("wakes that admitted a run = %d, want 1 (outcomes %v)", admitted, e.admitter.outcomes)
			}
			if failures := e.admitter.failures(); len(failures) != 0 {
				t.Fatalf("wakes failed: %v", failures)
			}
			delivered := e.sender.delivered()
			want := []string{
				"telegram/group-1: Alice, the freeze is Friday.",
				"telegram/group-1: Bob owns the release, noted.",
			}
			if !reflect.DeepEqual(delivered, want) {
				t.Fatalf("delivered = %q, want %q", delivered, want)
			}

			// The second run was shown everything since nobody answered it: the
			// earlier message as history, the two new ones, each from its author.
			requests := e.model.calls()
			if len(requests) != 4 {
				t.Fatalf("model calls = %d, want two for each run", len(requests))
			}
			if first := requestJSON(t, requests[0]); strings.Contains(first, "who owns the release?") {
				t.Fatalf("the first run saw a message that arrived after it began: %s", first)
			}
			last := requestJSON(t, requests[2])
			// Each message reaches the model as its own row, with its author,
			// and the run's own earlier answer is part of the history.
			for _, want := range []string{
				`sender=\"alice\"`, "is the freeze on this week?",
				"Alice, the freeze is Friday.",
				`sender=\"bob\" mentions_me=\"true\"`, "who owns the release?",
				`sender=\"carol\"`, "I do, since Tuesday",
			} {
				if !strings.Contains(last, want) {
					t.Fatalf("the second run was not shown %q: %s", want, last)
				}
			}

			// The run signed for the participant who wrote the newest input what the
			// inbound path signs for a turn answering that participant.
			carol := e.identityID("carol")
			scenario := routecredentials.Scenario{BotID: e.botID, RouteID: e.routeID, ChannelIdentityID: carol, BotOwnerUserID: e.ownerID}
			wantChat, wantSession := scenario.Want()
			gotChat, gotSession := e.credentialsOfRun(hw)
			inboundChat, inboundSession := e.processor.issueRouteCredentials(context.Background(),
				InboundIdentity{BotID: e.botID, ChannelIdentityID: carol}, e.botID, e.routeID,
				channel.InboundMessage{ReplyTarget: e2eTarget}, SessionResult{Type: sessionpkg.TypeDiscuss, Runtime: sessionpkg.RuntimeModel}, "")
			for name, pair := range map[string][3]map[string]string{
				"chat token":     {gotChat, routecredentials.Claims(t, e2eSigningKey, inboundChat), wantChat},
				"session bearer": {gotSession, routecredentials.Claims(t, e2eSigningKey, inboundSession), wantSession},
			} {
				if !reflect.DeepEqual(pair[0], pair[1]) || !reflect.DeepEqual(pair[0], pair[2]) {
					t.Fatalf("%s: discuss run %v, inbound %v, scenario %v", name, pair[0], pair[1], pair[2])
				}
			}

			// Who wrote what is recorded as the inbound path saw it, and the
			// mention it saw is what the admission reads.
			if senders := e.count(`SELECT count(DISTINCT sender_channel_identity_id) FROM bot_history_messages WHERE session_id = $1 AND role = 'user'`); senders != 3 {
				t.Fatalf("distinct senders in history = %d, want 3", senders)
			}
			if mentions := e.count(`SELECT count(*) FROM bot_session_events WHERE session_id = $1 AND (event_data->>'mentions_me')::boolean`); mentions != 1 {
				t.Fatalf("events that mention the bot = %d, want 1", mentions)
			}
		})
	}
}

func requestJSON(t *testing.T, req sdk.Request) string {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A message that names a runtime command in a session driving an External Agent
// runtime is answered by the turn the command starts, and by nothing else: the
// turn's end finds no discuss input left in the message it answered, and the
// input before it that nobody addressed to the bot stays unanswered until
// someone does.
func TestDiscussE2ERuntimeCommandIsAnsweredOnce(t *testing.T) {
	for _, deployment := range []e2eDeployment{e2eEmbedded, e2eSplit} {
		t.Run(string(deployment), func(t *testing.T) {
			codex := &e2eCodex{}
			e := newDiscussE2EWith(t, deployment, codex)
			e.bind("alice")
			e.bind("bob")
			e.bind("carol")

			e.mustSay("alice", "m1", "has the build finished?", false)
			eventually(t, "the wake for an unaddressed message to be answered", func() bool { return e.admitter.count(turn.DiscussSkipped) == 1 })
			if got := e.runs(); len(got) != 0 {
				t.Fatalf("runs for an unaddressed message = %v, want none", got)
			}

			e.mustSay("bob", "m2", "@memohbot /deploy now", true)
			settled()
			if got := e.runs(); got["completed"] != 1 || len(got) != 1 {
				t.Fatalf("runs after the command = %v, want the command's turn alone", got)
			}
			if prompts := codex.prompted(); len(prompts) != 1 || prompts[0].Command != "deploy" {
				t.Fatalf("runtime prompts = %+v, want the one command", prompts)
			}
			if got := e.count(`SELECT count(*) FROM session_runs WHERE session_id = $1 AND invocation_id LIKE 'discuss:%'`); got != 0 {
				t.Fatalf("discuss runs after the command = %d, want 0", got)
			}

			// Addressing the bot afterwards answers everything not yet answered.
			e.mustSay("carol", "m3", "@bot thanks", true)
			hw := e.position()
			eventually(t, "the addressed message to be answered", func() bool { return e.cover() == hw })
			if got := e.runs(); got["completed"] != 2 || len(got) != 1 {
				t.Fatalf("runs = %v, want the command's turn and one discuss run", got)
			}
		})
	}
}

// Members talking at once, each message delivered twice as a platform redelivers
// it, leave every message in history exactly once, and the session's last run
// is shown every one of them once.
func TestDiscussE2EConcurrentInboundKeepsEveryMessageOnce(t *testing.T) {
	for _, deployment := range []e2eDeployment{e2eEmbedded, e2eSplit} {
		t.Run(string(deployment), func(t *testing.T) {
			const perMember = 6
			members := []string{"alice", "bob", "carol", "dave"}
			e := newDiscussE2E(t, deployment)

			var wg sync.WaitGroup
			errs := make(chan error, len(members)*perMember*2)
			for _, member := range members {
				for i := range perMember {
					for range 2 {
						wg.Add(1)
						go func() {
							defer wg.Done()
							errs <- e.say(member, fmt.Sprintf("%s-%d", member, i), fmt.Sprintf("%s says number %02d", member, i), i%3 == 0)
						}()
					}
				}
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("HandleInbound() error = %v", err)
				}
			}

			total := len(members) * perMember
			eventually(t, "the session to answer everything", func() bool {
				runs := e.runs()
				return e.position() > 0 && e.cover() == e.position() && len(runs) == 1 && runs["completed"] > 0
			})

			if events := e.count(`SELECT count(*) FROM bot_session_events WHERE session_id = $1`); events != total {
				t.Fatalf("events = %d, want %d", events, total)
			}
			if rows := e.count(`SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND role = 'user' AND event_id IS NOT NULL`); rows != total {
				t.Fatalf("history rows = %d, want %d", rows, total)
			}
			if distinct := e.count(`SELECT count(DISTINCT source_message_id) FROM bot_history_messages WHERE session_id = $1 AND role = 'user'`); distinct != total {
				t.Fatalf("distinct messages in history = %d, want %d", distinct, total)
			}
			if failures := e.admitter.failures(); len(failures) != 0 {
				t.Fatalf("wakes failed: %v", failures)
			}

			calls := e.model.calls()
			last := requestJSON(t, calls[len(calls)-1])
			for _, member := range members {
				for i := range perMember {
					text := fmt.Sprintf("%s says number %02d", member, i)
					if got := strings.Count(last, text); got != 1 {
						t.Fatalf("the last run was shown %q %d times, want once", text, got)
					}
				}
			}
		})
	}
}

// Input nothing woke the server for is answered by the sweep: here the wake is
// taken away, and the sweep that runs when the server starts finds the session.
func TestDiscussE2ESweepAnswersWhatNoWakeDid(t *testing.T) {
	e := newDiscussE2E(t, e2eWithoutWake, &e2eStep{send: "Catching up."}, &e2eStep{})
	e.mustSay("alice", "m1", "is anyone there?", true)
	e.mustSay("bob", "m2", "the build is red", false)
	settled()
	if got := e.runs(); len(got) != 0 || e.cover() != 0 {
		t.Fatalf("runs = %v, cover = %d without a wake, want none", got, e.cover())
	}

	if err := e.service.StartDiscussSweep(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(e.service.StopDiscussSweep)
	hw := e.position()
	eventually(t, "the sweep to answer the recorded input", func() bool { return e.cover() == hw })

	want := []string{"telegram/group-1: Catching up."}
	if got := e.sender.delivered(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered = %q, want %q", got, want)
	}
	if got := e.runs(); got["completed"] != 1 || len(got) != 1 {
		t.Fatalf("runs = %v, want one completed", got)
	}
}
