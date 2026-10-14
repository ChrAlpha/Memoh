package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/bots"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/messaging"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/testutil/routecredentials"
)

// These tests drive discuss admission against the real PostgreSQL ledger and
// history: the cover position, the guard and the fence are all PostgreSQL's.
// The model is scripted; everything between the admission and the provider
// call is the application's own path. Each case is skipped unless
// TEST_POSTGRES_DSN names a database.

const discussTestSecret = "discuss-test-secret"

// discussStep is one scripted model call.
type discussStep struct {
	// reached closes when the call arrives; gate, when set, holds the answer
	// until the test closes it.
	reached chan struct{}
	gate    chan struct{}
	// send makes the call answer with a send tool call carrying this text.
	send string
	// text makes the call answer with this plain text.
	text string
	// fail makes the call fail.
	fail bool
}

// discussProvider answers each model call with the next scripted step, and a
// plain stop once the script is spent.
type discussProvider struct {
	mu       sync.Mutex
	steps    []*discussStep
	requests []sdk.Request
}

func (*discussProvider) Name() string                                    { return "discuss-script" }
func (*discussProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }
func (*discussProvider) Test(context.Context) error                      { return nil }
func (*discussProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (*discussProvider) DoGenerate(context.Context, sdk.Request) (sdk.ModelResult, error) {
	return sdk.ModelResult{FinishReason: sdk.FinishReasonStop}, nil
}

func (p *discussProvider) DoStream(ctx context.Context, req sdk.Request) (<-chan sdk.StreamPart, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	var step *discussStep
	if len(p.steps) > 0 {
		step, p.steps = p.steps[0], p.steps[1:]
	}
	p.mu.Unlock()
	if step == nil {
		step = &discussStep{}
	}
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
	if step.fail {
		return nil, errors.New("scripted provider failure")
	}
	ch := make(chan sdk.StreamPart, 8)
	ch <- &sdk.StartPart{}
	ch <- &sdk.StartStepPart{}
	if step.send != "" {
		ch <- &sdk.StreamToolCallPart{
			ToolCallID: "call-send-" + uuid.NewString(),
			ToolName:   tools.ToolSend().String(),
			Input:      toolexec.ArgumentsFromValue(map[string]any{"text": step.send}),
		}
		ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonToolCalls}
		ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonToolCalls}
	} else {
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

func (p *discussProvider) recorded() []sdk.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sdk.Request(nil), p.requests...)
}

// requestText is everything one model request carried, for substring checks.
func requestText(t *testing.T, req sdk.Request) string {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// discussSender records what the send tool delivered.
type discussSender struct {
	mu   sync.Mutex
	sent []discussSent
}

type discussSent struct {
	platform messaging.Platform
	target   string
	text     string
}

func (s *discussSender) Send(_ context.Context, _ string, platform messaging.Platform, req messaging.SendRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, discussSent{platform: platform, target: req.Target, text: req.Message.Text})
	return nil
}

func (s *discussSender) delivered() []discussSent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]discussSent(nil), s.sent...)
}

// discussStore answers the reads discuss admission and its run make against
// PostgreSQL; the rest stay with the chat fixture's store, so the run uses
// that fixture's model and provider.
type discussStore struct {
	dbstore.Queries
	pg *postgresstore.Queries
}

func (s discussStore) GetDiscussCoverPosition(ctx context.Context, p dbsqlc.GetDiscussCoverPositionParams) (int64, error) {
	return s.pg.GetDiscussCoverPosition(ctx, p)
}

func (s discussStore) GetDiscussPendingHead(ctx context.Context, p dbsqlc.GetDiscussPendingHeadParams) (dbsqlc.GetDiscussPendingHeadRow, error) {
	return s.pg.GetDiscussPendingHead(ctx, p)
}

func (s discussStore) GetDiscussPendingAddressed(ctx context.Context, p dbsqlc.GetDiscussPendingAddressedParams) (bool, error) {
	return s.pg.GetDiscussPendingAddressed(ctx, p)
}

func (s discussStore) ListDiscussSweepCandidates(ctx context.Context, p dbsqlc.ListDiscussSweepCandidatesParams) ([]dbsqlc.ListDiscussSweepCandidatesRow, error) {
	return s.pg.ListDiscussSweepCandidates(ctx, p)
}

func (s discussStore) GetDiscussSession(ctx context.Context, p dbsqlc.GetDiscussSessionParams) (dbsqlc.GetDiscussSessionRow, error) {
	return s.pg.GetDiscussSession(ctx, p)
}

func (s discussStore) ListUserIDsByChannelIdentity(ctx context.Context, id pgtype.UUID) ([]pgtype.UUID, error) {
	return s.pg.ListUserIDsByChannelIdentity(ctx, id)
}

func (s discussStore) GetBotByID(ctx context.Context, id pgtype.UUID) (dbsqlc.GetBotByIDRow, error) {
	return s.pg.GetBotByID(ctx, id)
}

func (s discussStore) GetChannelIdentityByID(ctx context.Context, id pgtype.UUID) (dbsqlc.ChannelIdentity, error) {
	return s.pg.GetChannelIdentityByID(ctx, id)
}

func (s discussStore) GetUserByID(ctx context.Context, id pgtype.UUID) (dbsqlc.User, error) {
	return s.pg.GetUserByID(ctx, id)
}

func (s discussStore) SaveSessionRunResumeContext(ctx context.Context, p dbsqlc.SaveSessionRunResumeContextParams) (int64, error) {
	return s.pg.SaveSessionRunResumeContext(ctx, p)
}

// discussEnv is one discuss session in a group conversation, reached through a
// channel route, with the bot's owner.
type discussEnv struct {
	pool      *pgxpool.Pool
	botID     string
	sessionID string
	routeID   string
	ownerID   string
}

const (
	discussTestPlatform = "telegram"
	discussTestTarget   = "group-1"
)

func newDiscussEnv(t *testing.T, conversationType string) discussEnv {
	t.Helper()
	ctx := context.Background()
	pool := openTurnAdmissionPostgres(t, ctx)
	botID, sessionID := createTurnAdmissionFixture(t, ctx, pool)
	seedDirectLifecycleModel(t, ctx, pool)
	env := discussEnv{pool: pool, botID: botID, sessionID: sessionID, routeID: uuid.NewString()}
	if err := pool.QueryRow(ctx, `SELECT owner_user_id::text FROM bots WHERE id = $1`, botID).Scan(&env.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_channel_routes (id, bot_id, channel_type, external_conversation_id, conversation_type, default_reply_target)
		VALUES ($1, $2, $3, $4, $5, $4)
	`, env.routeID, botID, discussTestPlatform, discussTestTarget, conversationType); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE bot_sessions
		SET type = 'discuss', session_mode = 'discuss', route_id = $2,
		    reply_delivery = 'explicit', trigger_scope = 'uncovered_events'
		WHERE id = $1
	`, sessionID, env.routeID); err != nil {
		t.Fatalf("make the session a discuss session: %v", err)
	}
	return env
}

// discussParticipant is an external sender, optionally bound to an account.
type discussParticipant struct {
	identityID string
	userID     string
}

func (e discussEnv) participant(t *testing.T, bound bool) discussParticipant {
	t.Helper()
	ctx := context.Background()
	p := discussParticipant{identityID: uuid.NewString()}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO channel_identities (id, channel_type, channel_subject_id, display_name)
		VALUES ($1, $2, $3, 'participant')
	`, p.identityID, discussTestPlatform, "subject-"+p.identityID); err != nil {
		t.Fatalf("create channel identity: %v", err)
	}
	if !bound {
		return p
	}
	p.userID = uuid.NewString()
	if _, err := e.pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'member' FROM created_user
	`, p.userID, "participant-"+p.userID); err != nil {
		t.Fatalf("create participant account: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO user_channel_identity_bindings (user_id, channel_identity_id) VALUES ($1, $2)
	`, p.userID, p.identityID); err != nil {
		t.Fatalf("bind participant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.WithoutCancel(ctx), "DELETE FROM users WHERE id = $1", p.userID)
	})
	return p
}

// passiveInput is one external message as the inbound path writes it.
type passiveInput struct {
	from       discussParticipant
	externalID string
	text       string
	mentionsMe bool
	// refused writes the row the way a refused sender's message is written:
	// without an event, marked with the admission that refused it.
	refused messagepkg.Admission
}

// write stores the input and returns its history position. An admitted input
// is a session event and the history row that carries its id.
func (e discussEnv) write(t *testing.T, in passiveInput) int64 {
	t.Helper()
	ctx := context.Background()
	modelMessage := sdkMessagesToModelMessages([]sdk.Message{sdk.UserMessage(in.text)})[0]
	raw, err := json.Marshal(modelMessage)
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"route_id": e.routeID, "platform": discussTestPlatform}
	if in.refused != "" {
		messagepkg.SetAdmission(meta, in.refused)
	}
	svc := messagepkg.NewService(slog.New(slog.DiscardHandler), postgresstore.NewQueriesWithPool(e.pool, dbsqlc.New(e.pool)))
	row, err := svc.Persist(ctx, messagepkg.PersistInput{
		BotID: e.botID, SessionID: e.sessionID, Role: "user", Content: raw,
		SenderChannelIdentityID: in.from.identityID, ExternalMessageID: in.externalID,
		DisplayText: in.text, Metadata: meta,
	})
	if err != nil {
		t.Fatalf("persist %s: %v", in.externalID, err)
	}
	if in.refused == "" {
		eventData, err := json.Marshal(map[string]any{"message_id": in.externalID, "mentions_me": in.mentionsMe})
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.New()
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO bot_session_events (id, bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms)
			VALUES ($1, $2, $3, 'message', $4, $5, $6)
		`, eventID, e.botID, e.sessionID, eventData, in.externalID, time.Now().UnixMilli()); err != nil {
			t.Fatalf("insert event %s: %v", in.externalID, err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE bot_history_messages SET event_id = $2 WHERE id = $1`, row.ID, eventID); err != nil {
			t.Fatal(err)
		}
	}
	var position int64
	if err := e.pool.QueryRow(ctx, `SELECT turn_position FROM bot_history_messages WHERE id = $1`, row.ID).Scan(&position); err != nil {
		t.Fatal(err)
	}
	return position
}

// discussRun is one discuss run as the ledger holds it.
type discussRun struct {
	runID         string
	state         string
	coverPosition int64
	input         map[string]json.RawMessage
}

// runs lists the session's discuss runs by invocation id.
func (e discussEnv) runs(t *testing.T) map[string]discussRun {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT invocation_id, run_id::text, state, COALESCE((input_json->>'cover_position')::bigint, 0), input_json
		FROM session_runs WHERE session_id = $1 AND invocation_id LIKE 'discuss:%'
	`, e.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]discussRun{}
	for rows.Next() {
		var invocation string
		var run discussRun
		var raw []byte
		if err := rows.Scan(&invocation, &run.runID, &run.state, &run.coverPosition, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &run.input); err != nil {
			t.Fatal(err)
		}
		out[invocation] = run
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e discussEnv) invocation(hw int64) string { return discussInvocationID(e.sessionID, hw) }

// waitRunState polls until the run for invocation reaches state.
func (e discussEnv) waitRunState(t *testing.T, invocation, state string) discussRun {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if run, ok := e.runs(t)[invocation]; ok && run.state == state {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach %s; runs = %v", invocation, state, e.runs(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e discussEnv) cover(t *testing.T) int64 {
	t.Helper()
	cover, err := dbsqlc.New(e.pool).GetDiscussCoverPosition(context.Background(), dbsqlc.GetDiscussCoverPositionParams{
		SessionID: db.ParseUUIDOrEmpty(e.sessionID), States: discussCoverStates,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cover
}

// discussProcess is one server process: its own session runtime over the
// shared ledger, its own model and its own send tool.
type discussProcess struct {
	svc      *Service
	manager  *sessionruntime.Manager
	provider *discussProvider
	sender   *discussSender
}

func newDiscussProcess(t *testing.T, env discussEnv, steps ...*discussStep) *discussProcess {
	t.Helper()
	chat := newChatTurnFixture(t, directLifecycleModelSuccess)
	svc := chat.fixture.service
	base, ok := svc.queries.(noDecisionQueries)
	if !ok {
		t.Fatalf("fixture queries = %T", svc.queries)
	}
	// The run offers tools only to a model that can call them.
	for key, model := range base.models {
		model.Config = []byte(`{"context_window":128000,"compatibilities":["tool-call"]}`)
		base.models[key] = model
	}
	pg := postgresstore.NewQueriesWithPool(env.pool, dbsqlc.New(env.pool))
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-discuss-" + uuid.NewString(),
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(env.pool), env.pool),
		Fence:         runtimefence.NewActivator(pg),
	})
	t.Cleanup(func() { _ = manager.Close() })
	svc.SetSessionRuntime(manager)
	svc.messageService = messagepkg.NewService(svc.logger, pg)
	svc.queries = discussStore{Queries: base, pg: pg}
	svc.SetResumeSecret(discussTestSecret)
	svc.allowedTeam = ""
	svc.sessionService = &fakeBackgroundSessionService{
		getFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{
				ID: id, BotID: env.botID, Type: sessionpkg.TypeDiscuss,
				SessionMode: sessionpkg.TypeDiscuss, RuntimeType: sessionpkg.RuntimeModel,
				Facts: sessionfacts.Preset(sessionpkg.TypeDiscuss),
			}, nil
		},
	}

	provider := &discussProvider{steps: steps}
	sender := &discussSender{}
	logger := slog.New(slog.DiscardHandler)
	applier := contextview.ProviderRunConfigApplier(logger)
	agent := native.New(native.Deps{
		Logger: logger,
		ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) native.RunConfig {
			out := applier(ctx, cfg)
			if out.Model != nil {
				model := *out.Model
				model.Provider = provider
				out.Model = &model
			}
			return out
		},
	})
	agent.SetToolProviders([]tools.ToolProvider{
		tools.NewMessageProvider(nil, sender, goldenMessagingReactor{}, goldenChannelResolver{}, nil),
	})
	svc.agent = agent
	svc.userInput = &fakeUserInputService{}
	return &discussProcess{svc: svc, manager: manager, provider: provider, sender: sender}
}

func (p *discussProcess) admit(t *testing.T, env discussEnv) (DiscussAdmission, <-chan struct{}) {
	t.Helper()
	outcome, done, err := p.svc.admitDiscuss(context.Background(), env.botID, env.sessionID)
	if err != nil {
		t.Fatalf("admitDiscuss() error = %v", err)
	}
	return outcome, done
}

func waitDiscussRun(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done == nil {
		t.Fatal("no admitted run to wait for")
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("discuss run did not end")
	}
}

func waitReached(t *testing.T, step *discussStep) {
	t.Helper()
	select {
	case <-step.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not reach the scripted model call")
	}
}

// A first external input is answered by one run with nothing before it. The
// run speaks only through the send tool, into the route's conversation, and
// acts for the participant who wrote the input; once it completes the input is
// covered.
func TestDiscussAnswersPendingInputThroughSend(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, true)
	proc := newDiscussProcess(t, env, &discussStep{send: "The freeze is Friday."})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "is the freeze on this week?"})

	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)

	run := env.waitRunState(t, env.invocation(hw), "completed")
	if run.coverPosition != hw {
		t.Fatalf("run cover_position = %d, want %d", run.coverPosition, hw)
	}
	if got := env.cover(t); got != hw {
		t.Fatalf("cover position = %d, want %d", got, hw)
	}
	sent := proc.sender.delivered()
	if len(sent) != 1 || sent[0].platform != discussTestPlatform || sent[0].target != discussTestTarget || !strings.Contains(sent[0].text, "The freeze is Friday.") {
		t.Fatalf("delivered = %+v, want one send into the route's conversation", sent)
	}
	requests := proc.provider.recorded()
	if len(requests) == 0 {
		t.Fatal("the run never reached the model")
	}
	first := requestText(t, requests[0])
	if !strings.Contains(first, "is the freeze on this week?") {
		t.Fatalf("the model was not shown the input: %s", first)
	}
	// Explicit delivery: the model is told that not calling send is silence.
	if !strings.Contains(first, "if you do not call it, you stay silent") {
		t.Fatalf("the run was not told it speaks through send: %s", first)
	}

	// The credentials name the participant who wrote the input and the route
	// it came in on; the session acts for the bot's owner. They are recorded
	// for a later resume like any other run's, and are what the inbound path
	// signs for the same participant: its tests hold it to the same scenario.
	var resume resumeContext
	if err := json.Unmarshal(run.input["resume"], &resume); err != nil {
		t.Fatalf("resume context: %v (%s)", err, run.input["resume"])
	}
	wantChat, wantSession := routecredentials.Scenario{
		BotID: env.botID, RouteID: env.routeID, ChannelIdentityID: alice.identityID,
		ParticipantUserID: alice.userID, BotOwnerUserID: env.ownerID,
	}.Want()
	if !reflect.DeepEqual(resume.ChatClaims, wantChat) || !reflect.DeepEqual(resume.TokenClaims, wantSession) {
		t.Fatalf("run credentials\nchat    %v\nsession %v\nwant\nchat    %v\nsession %v", resume.ChatClaims, resume.TokenClaims, wantChat, wantSession)
	}
	if resume.ChannelIdentityID != alice.identityID || resume.ReplyTarget != discussTestTarget || resume.CurrentChannel != discussTestPlatform {
		t.Fatalf("resume context = %+v", resume)
	}

	if outcome, _ := proc.admit(t, env); outcome != DiscussNothingPending {
		t.Fatalf("check after completion = %v, want nothing pending", outcome)
	}
}

// Which sessions are answered this way is the session's trigger scope. A
// session named discuss whose runs start from an addressed message is not, and
// one named chat whose runs answer every uncovered event is.
func TestDiscussAdmissionReadsTriggerScope(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("addressed message", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		if _, err := env.pool.Exec(context.Background(), `
			UPDATE bot_sessions SET reply_delivery = 'stream', trigger_scope = 'addressed_message' WHERE id = $1
		`, env.sessionID); err != nil {
			t.Fatal(err)
		}
		proc := newDiscussProcess(t, env)
		env.write(t, passiveInput{from: env.participant(t, false), externalID: "m1", text: "a message"})
		if outcome, _ := proc.admit(t, env); outcome != DiscussSkipped {
			t.Fatalf("outcome = %v, want skipped", outcome)
		}
		if got := len(env.runs(t)); got != 0 {
			t.Fatalf("runs = %d, want 0", got)
		}
	})
	t.Run("uncovered events", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		if _, err := env.pool.Exec(context.Background(), `
			UPDATE bot_sessions SET type = 'chat', session_mode = 'chat' WHERE id = $1
		`, env.sessionID); err != nil {
			t.Fatal(err)
		}
		proc := newDiscussProcess(t, env)
		hw := env.write(t, passiveInput{from: env.participant(t, false), externalID: "m1", text: "a message"})
		outcome, done := proc.admit(t, env)
		if outcome != DiscussAdmitted {
			t.Fatalf("outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(hw), "completed")
	})
}

// A participant bound to more than one account names no account, as on the
// inbound path: the chat token names the channel identity alone.
func TestDiscussAmbiguousParticipantNamesNoAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	bob := env.participant(t, true)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO user_channel_identity_bindings (user_id, channel_identity_id) VALUES ($1, $2)
	`, env.ownerID, bob.identityID); err != nil {
		t.Fatal(err)
	}
	proc := newDiscussProcess(t, env)
	hw := env.write(t, passiveInput{from: bob, externalID: "m1", text: "who am I"})
	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	run := env.waitRunState(t, env.invocation(hw), "completed")
	var resume resumeContext
	if err := json.Unmarshal(run.input["resume"], &resume); err != nil {
		t.Fatal(err)
	}
	if resume.UserID != "" || resume.ChatClaims["user_id"] != bob.identityID {
		t.Fatalf("ambiguous participant: user_id = %q, chat claims = %v, want no account", resume.UserID, resume.ChatClaims)
	}
}

// Only a completed run covers its batch. After the batch's run is aborted,
// failed or lost, the same batch is not admitted again; a newer input is, and
// its run answers both.
func TestDiscussUnchangedBatchIsNotAdmittedAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	for _, terminal := range []string{"aborted", "failed", "lost"} {
		t.Run(terminal, func(t *testing.T) {
			env := newDiscussEnv(t, "group")
			alice := env.participant(t, false)
			proc := newDiscussProcess(t, env)
			first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "first batch"})
			outcome, done := proc.admit(t, env)
			if outcome != DiscussAdmitted {
				t.Fatalf("first outcome = %v, want admitted", outcome)
			}
			waitDiscussRun(t, done)
			env.waitRunState(t, env.invocation(first), "completed")

			if _, err := env.pool.Exec(context.Background(), `UPDATE session_runs SET state = $2 WHERE session_id = $1 AND invocation_id = $3`, env.sessionID, terminal, env.invocation(first)); err != nil {
				t.Fatal(err)
			}
			if got := env.cover(t); got != 0 {
				t.Fatalf("cover after %s = %d, want 0", terminal, got)
			}
			for range 2 {
				if outcome, _ := proc.admit(t, env); outcome != DiscussSkipped {
					t.Fatalf("unchanged batch after %s: outcome = %v, want skipped", terminal, outcome)
				}
			}
			if got := len(env.runs(t)); got != 1 {
				t.Fatalf("runs after the unchanged batch = %d, want 1", got)
			}

			second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "second batch"})
			outcome, done = proc.admit(t, env)
			if outcome != DiscussAdmitted {
				t.Fatalf("after a new input: outcome = %v, want admitted", outcome)
			}
			waitDiscussRun(t, done)
			env.waitRunState(t, env.invocation(second), "completed")
			if got := env.cover(t); got != second {
				t.Fatalf("cover = %d, want %d", got, second)
			}
			last := requestText(t, proc.provider.recorded()[len(proc.provider.recorded())-1])
			if !strings.Contains(last, "first batch") || !strings.Contains(last, "second batch") {
				t.Fatalf("the retry did not see both batches: %s", last)
			}
		})
	}
}

// Two processes checking one session at once admit one round between them:
// the PostgreSQL ledger and its fence arbitrate, not process memory.
func TestDiscussTwoManagersAdmitOneRound(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	procs := []*discussProcess{newDiscussProcess(t, env), newDiscussProcess(t, env)}
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "race"})

	const perProcess = 3
	type result struct {
		outcome DiscussAdmission
		done    <-chan struct{}
		err     error
	}
	results := make([]result, len(procs)*perProcess)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		proc := procs[i%len(procs)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := &results[i]
			r.outcome, r.done, r.err = proc.svc.admitDiscuss(context.Background(), env.botID, env.sessionID)
		}()
	}
	close(start)
	wg.Wait()

	admitted := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
		if r.outcome == DiscussAdmitted {
			admitted++
			waitDiscussRun(t, r.done)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted = %d, want 1", admitted)
	}
	env.waitRunState(t, env.invocation(hw), "completed")
	if got := len(env.runs(t)); got != 1 {
		t.Fatalf("discuss runs = %d, want 1", got)
	}
	calls := len(procs[0].provider.recorded()) + len(procs[1].provider.recorded())
	if calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
}

// An External Agent runtime in a group runs only once the pending input
// addresses the bot. Unaddressed input writes no run and stays pending; the
// first addressed input answers everything before it. A private conversation
// always addresses the bot.
func TestDiscussExternalRuntimeRunsOnlyWhenAddressed(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("group", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		alice := env.participant(t, false)
		if _, err := env.pool.Exec(context.Background(), `UPDATE bot_sessions SET runtime_type = 'codex' WHERE id = $1`, env.sessionID); err != nil {
			t.Fatal(err)
		}
		proc := newDiscussProcess(t, env)
		env.write(t, passiveInput{from: alice, externalID: "m1", text: "not for the bot"})
		if outcome, _ := proc.admit(t, env); outcome != DiscussSkipped {
			t.Fatalf("unaddressed outcome = %v, want skipped", outcome)
		}
		if got := len(env.runs(t)); got != 0 {
			t.Fatalf("runs for unaddressed input = %d, want 0", got)
		}
		hw := env.write(t, passiveInput{from: alice, externalID: "m2", text: "@bot over to you", mentionsMe: true})
		outcome, done := proc.admit(t, env)
		if outcome != DiscussAdmitted {
			t.Fatalf("addressed outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
		if _, ok := env.runs(t)[env.invocation(hw)]; !ok {
			t.Fatalf("runs = %v, want the addressed batch's run", env.runs(t))
		}
	})
	t.Run("private", func(t *testing.T) {
		env := newDiscussEnv(t, "private")
		alice := env.participant(t, false)
		if _, err := env.pool.Exec(context.Background(), `UPDATE bot_sessions SET runtime_type = 'codex' WHERE id = $1`, env.sessionID); err != nil {
			t.Fatal(err)
		}
		proc := newDiscussProcess(t, env)
		env.write(t, passiveInput{from: alice, externalID: "m1", text: "a direct message"})
		outcome, done := proc.admit(t, env)
		if outcome != DiscussAdmitted {
			t.Fatalf("private outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
	})
}

// A message from a sender refused at admission is neither pending input nor
// part of the model's context.
func TestDiscussRefusedInputIsNotAnsweredOrShown(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	mallory := env.participant(t, false)
	proc := newDiscussProcess(t, env)

	env.write(t, passiveInput{from: mallory, externalID: "r0", text: "refused before anything", refused: messagepkg.AdmissionRejectedACL})
	if outcome, _ := proc.admit(t, env); outcome != DiscussNothingPending {
		t.Fatalf("only refused input: outcome = %v, want nothing pending", outcome)
	}

	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "an admitted question"})
	env.write(t, passiveInput{from: mallory, externalID: "r1", text: "refused after it", refused: messagepkg.AdmissionRejectedACL})
	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	run := env.waitRunState(t, env.invocation(hw), "completed")
	if run.coverPosition != hw {
		t.Fatalf("cover_position = %d, want the admitted row's %d", run.coverPosition, hw)
	}
	text := requestText(t, proc.provider.recorded()[0])
	if !strings.Contains(text, "an admitted question") {
		t.Fatalf("the admitted input is missing: %s", text)
	}
	if strings.Contains(text, "refused before anything") || strings.Contains(text, "refused after it") {
		t.Fatalf("refused input reached the model: %s", text)
	}
}

// A discuss run commits each step as it goes, and a run that fails leaves a
// failure row against the input it was answering.
func TestDiscussCommitsStepsAndRecordsFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("steps", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		alice := env.participant(t, false)
		second := &discussStep{reached: make(chan struct{}), gate: make(chan struct{})}
		proc := newDiscussProcess(t, env, &discussStep{send: "on it"}, second)
		hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "please check"})
		outcome, done := proc.admit(t, env)
		if outcome != DiscussAdmitted {
			t.Fatalf("outcome = %v, want admitted", outcome)
		}
		waitReached(t, second)
		// The send step is in history while the run is still on its next call.
		var toolRows int
		if err := env.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND role = 'tool'
		`, env.sessionID).Scan(&toolRows); err != nil {
			t.Fatal(err)
		}
		if run := env.runs(t)[env.invocation(hw)]; run.state != "running" {
			t.Fatalf("run state while held = %q, want running", run.state)
		}
		if toolRows != 1 {
			t.Fatalf("tool rows while the run is still going = %d, want the committed send step", toolRows)
		}
		close(second.gate)
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(hw), "completed")
	})
	t.Run("failure", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		alice := env.participant(t, false)
		proc := newDiscussProcess(t, env, &discussStep{fail: true})
		hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "please check"})
		outcome, done := proc.admit(t, env)
		if outcome != DiscussAdmitted {
			t.Fatalf("outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(hw), "failed")
		if got := env.cover(t); got != 0 {
			t.Fatalf("cover after a failure = %d, want 0", got)
		}
		var users, failures int
		if err := env.pool.QueryRow(context.Background(), `
			SELECT count(*) FILTER (WHERE role = 'user'),
			       count(*) FILTER (WHERE COALESCE(metadata->>$2, '') <> '')
			FROM bot_history_messages WHERE session_id = $1
		`, env.sessionID, messagepkg.HistoryErrorCodeMetadataKey).Scan(&users, &failures); err != nil {
			t.Fatal(err)
		}
		if users != 1 || failures != 1 {
			t.Fatalf("history after failure: user rows = %d, failure rows = %d, want 1 and 1", users, failures)
		}
	})
}

// A discuss run interrupted by a shutdown continues as a resume run that
// carries the interrupted run's cover position, so the batch is covered once
// the continuation completes.
func TestDiscussResumeKeepsCoverPosition(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{send: "starting"})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "long task"})
	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	interrupted := env.waitRunState(t, env.invocation(hw), "completed")
	// The run was cut off by a shutdown instead.
	if _, err := env.pool.Exec(ctx, `
		UPDATE session_runs SET state = 'lost', error_code = 'session_runtime.interrupted' WHERE run_id = $1
	`, interrupted.runID); err != nil {
		t.Fatal(err)
	}
	if got := env.cover(t); got != 0 {
		t.Fatalf("cover with the run interrupted = %d, want 0", got)
	}

	rows, err := dbsqlc.New(env.pool).ListInterruptedSessionRuns(ctx, db.ParseUUIDOrEmpty(uuid.Nil.String()))
	if err != nil {
		t.Fatal(err)
	}
	var row dbsqlc.SessionRun
	for _, candidate := range rows {
		if candidate.RunID.String() == interrupted.runID {
			row = candidate
		}
	}
	if !row.RunID.Valid {
		t.Fatalf("the interrupted run is not listed for resume")
	}
	proc.svc.SetAllowedTeam(row.TeamID.String())
	proc.svc.accountService = accounts.NewService(nil, titleModelAccountStore{account: dbstore.AccountRecord{ID: env.ownerID, IsActive: true}})
	proc.svc.botPermissions = &fakeBotPermissionChecker{values: map[string]bool{env.botID + ":" + env.ownerID + ":" + bots.PermissionChat: true}}
	resumed, err := proc.svc.resumeInterruptedSession(ctx, row)
	if err != nil {
		t.Fatalf("resumeInterruptedSession() error = %v", err)
	}
	waitDiscussRun(t, resumed)

	var state string
	var cover int64
	if err := env.pool.QueryRow(ctx, `
		SELECT state, COALESCE((input_json->>'cover_position')::bigint, 0) FROM session_runs WHERE invocation_id = $1
	`, "resume:"+interrupted.runID).Scan(&state, &cover); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || cover != hw {
		t.Fatalf("resume run state = %q cover_position = %d, want completed and %d", state, cover, hw)
	}
	// The continuation still sees the input it was answering.
	requests := proc.provider.recorded()
	if last := requestText(t, requests[len(requests)-1]); !strings.Contains(last, "long task") || !strings.Contains(last, "gracefully shut down") {
		t.Fatalf("the resumed run lost its input or instruction: %s", last)
	}
	if got := env.cover(t); got != hw {
		t.Fatalf("cover after the resume = %d, want %d", got, hw)
	}
	if outcome, _ := proc.admit(t, env); outcome != DiscussNothingPending {
		t.Fatalf("check after the resume = %v, want nothing pending", outcome)
	}
}

// Input that arrives while a run holds the session is refused as busy, and the
// run's end answers it.
func TestDiscussRunEndAnswersInputThatFoundItBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	held := &discussStep{reached: make(chan struct{}), gate: make(chan struct{})}
	proc := newDiscussProcess(t, env, held)
	env.write(t, passiveInput{from: alice, externalID: "m1", text: "first"})
	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitReached(t, held)

	second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "arrived while busy"})
	if outcome, _ := proc.admit(t, env); outcome != DiscussBusy {
		t.Fatalf("check while held = %v, want busy", outcome)
	}
	close(held.gate)
	waitDiscussRun(t, done)

	env.waitRunState(t, env.invocation(second), "completed")
	requests := proc.provider.recorded()
	if !strings.Contains(requestText(t, requests[len(requests)-1]), "arrived while busy") {
		t.Fatal("the follow-up run did not see the input that found the session busy")
	}
}

// racingAdmitter lets a test act between a discuss check's reads and its
// admission.
type racingAdmitter struct {
	*sessionruntime.Manager
	once   sync.Once
	before func()
}

func (r *racingAdmitter) Admit(ctx context.Context, in sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	r.once.Do(r.before)
	return r.Manager.Admit(ctx, in)
}

// A check whose cover position went stale before its admission ends its run
// without calling the model: a newer batch's run already answered its input.
func TestDiscussStaleBatchEndsWithoutAnswering(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	slow := newDiscussProcess(t, env)
	fast := newDiscussProcess(t, env)
	first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "first"})
	var second int64
	slow.svc.sessionRuntime = &racingAdmitter{Manager: slow.manager, before: func() {
		second = env.write(t, passiveInput{from: alice, externalID: "m2", text: "second"})
		outcome, done := fast.admit(t, env)
		if outcome != DiscussAdmitted {
			t.Errorf("newer batch outcome = %v, want admitted", outcome)
			return
		}
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(second), "completed")
	}}

	if outcome, done := slow.admit(t, env); outcome != DiscussNothingPending || done != nil {
		t.Fatalf("stale batch outcome = %v, want nothing pending", outcome)
	}
	env.waitRunState(t, env.invocation(first), "completed")
	if calls := len(slow.provider.recorded()); calls != 0 {
		t.Fatalf("the stale batch called the model %d times, want 0", calls)
	}
	if got := env.cover(t); got != second {
		t.Fatalf("cover = %d, want the newer batch's %d", got, second)
	}
}
