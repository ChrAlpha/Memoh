package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/pending"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/logger"
	"github.com/felinics/memoh/internal/messaging"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/team"
)

// admitCommand is the check the channel side asks for, over the env's session.
func (e discussEnv) admitCommand() turn.DiscussAdmitCommand {
	return turn.DiscussAdmitCommand{TeamID: team.DefaultTeamID, BotID: e.botID, ThreadID: e.sessionID}
}

// A message that names a runtime command in a discuss session is not passive
// input: the inbound path starts a chat turn for it, and the turn writes the
// message as its request row, with the event. These tests drive that turn
// through the application against the real ledger and history, then ask the
// discuss admission what is left. The session is an External Agent runtime's in
// the database; the turn itself runs on the scripted native model.

// chatTurn starts the chat turn the inbound path starts for an addressed
// message: its event is recorded without a row, and the turn writes the row.
func (p *discussProcess) chatTurn(t *testing.T, env discussEnv, from discussParticipant, externalID, text string) (eventID string, handle turn.RunHandle) {
	t.Helper()
	eventID = uuid.NewString()
	eventData, err := json.Marshal(map[string]any{"message_id": externalID, "mentions_me": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO bot_session_events (id, bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms, mentions_me)
		VALUES ($1, $2, $3, 'message', $4, $5, $6, true)
	`, eventID, env.botID, env.sessionID, eventData, externalID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert event %s: %v", externalID, err)
	}
	handle, err = p.svc.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "team-test",
		Mode:                    turn.ModeChat,
		BotID:                   env.botID,
		ChatID:                  env.botID,
		ThreadID:                env.sessionID,
		RouteID:                 env.routeID,
		IdempotencyKey:          "chat:" + externalID,
		ExternalMessageID:       externalID,
		EventID:                 eventID,
		Query:                   text,
		UserVisibleText:         text,
		MentionsBot:             true,
		SourceChannelIdentityID: from.identityID,
		CurrentChannel:          discussTestPlatform,
		ReplyTarget:             discussTestTarget,
		ConversationType:        "group",
	})
	if err != nil {
		t.Fatalf("StartTurn(%s) error = %v", externalID, err)
	}
	return eventID, handle
}

// allRuns counts every run of the session, whatever started it.
func (e discussEnv) allRuns(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM session_runs WHERE session_id = $1`, e.sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requestRow is the history row a chat turn wrote for its request.
func (e discussEnv) requestRow(t *testing.T, eventID string) (turnID, runID string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `
		SELECT turn_id::text, run_id::text FROM bot_history_messages WHERE session_id = $1 AND event_id = $2 AND role = 'user'
	`, e.sessionID, eventID).Scan(&turnID, &runID); err != nil {
		t.Fatalf("the turn wrote no request row for event %s: %v", eventID, err)
	}
	return turnID, runID
}

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

func (*discussProvider) Name() string { return "discuss-script" }

func (*discussProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*discussProvider) Test(context.Context) error { return nil }

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

func (s discussStore) ListDiscussPendingInputs(ctx context.Context, p dbsqlc.ListDiscussPendingInputsParams) ([]dbsqlc.ListDiscussPendingInputsRow, error) {
	return s.pg.ListDiscussPendingInputs(ctx, p)
}

// The compaction reads of a run whose history is summarized as it goes.
func (s discussStore) ListCompactionArtifactLineageBySession(ctx context.Context, id pgtype.UUID) ([]dbsqlc.BotHistoryMessageCompact, error) {
	return s.pg.ListCompactionArtifactLineageBySession(ctx, id)
}

func (s discussStore) GetCompactionLogByID(ctx context.Context, id pgtype.UUID) (dbsqlc.BotHistoryMessageCompact, error) {
	return s.pg.GetCompactionLogByID(ctx, id)
}

func (s discussStore) ListCompactionArtifactParentIDsBySuccessor(ctx context.Context, p dbsqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error) {
	return s.pg.ListCompactionArtifactParentIDsBySuccessor(ctx, p)
}

func (s discussStore) ListMessageRefsByCompactID(ctx context.Context, id pgtype.UUID) ([]dbsqlc.ListMessageRefsByCompactIDRow, error) {
	return s.pg.ListMessageRefsByCompactID(ctx, id)
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

func (s discussStore) ListInterruptedSessionRuns(ctx context.Context, after pgtype.UUID) ([]dbsqlc.SessionRun, error) {
	return s.pg.ListInterruptedSessionRuns(ctx, after)
}

func (s discussStore) GetSessionInterruptedRun(ctx context.Context, sessionID pgtype.UUID) (dbsqlc.SessionRun, error) {
	return s.pg.GetSessionInterruptedRun(ctx, sessionID)
}

func (s discussStore) RetireSupersededInterruptedSessionRuns(ctx context.Context) (int64, error) {
	return s.pg.RetireSupersededInterruptedSessionRuns(ctx)
}

func (s discussStore) RetireInterruptedSessionRun(ctx context.Context, runID pgtype.UUID) (int64, error) {
	return s.pg.RetireInterruptedSessionRun(ctx, runID)
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
	pool := dbtest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)
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

// setRuntime makes the session run on runtimeType. A session an External Agent
// runtime drives acts for an owner, recorded as creating the session does: in
// the owner column and in both metadata keys.
func (e discussEnv) setRuntime(t *testing.T, runtimeType string) {
	t.Helper()
	if runtimeType == "model" {
		if _, err := e.pool.Exec(context.Background(), `UPDATE bot_sessions SET runtime_type = 'model' WHERE id = $1`, e.sessionID); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE bot_sessions
		SET runtime_type = $2,
		    metadata = metadata || jsonb_build_object('runtime_owner_account_id', $3::text),
		    runtime_metadata = runtime_metadata || jsonb_build_object('runtime_owner_account_id', $3::text),
		    runtime_owner_user_id = $3::uuid
		WHERE id = $1
	`, e.sessionID, runtimeType, e.ownerID); err != nil {
		t.Fatal(err)
	}
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
	// repliesToMe is a reply to one of the bot's own messages.
	repliesToMe bool
	// refused writes the row the way a refused sender's message is written:
	// without an event, marked with the admission that refused it.
	refused messagepkg.Admission
	// assets are the images and files linked to the row, as the inbound path
	// links what the message carried.
	assets []messagepkg.AssetRef
}

// write stores the input and returns its history position. An admitted input
// is a session event and the history row that carries its id.
func (e discussEnv) write(t *testing.T, in passiveInput) int64 {
	t.Helper()
	return e.writeAll(t, in)[0]
}

// writeAll stores the inputs and returns their history positions. The inputs
// become external input together: a check that runs meanwhile, such as the
// one a run's end makes, sees all of them or none.
func (e discussEnv) writeAll(t *testing.T, inputs ...passiveInput) []int64 {
	t.Helper()
	ctx := context.Background()
	svc := messagepkg.NewService(slog.New(slog.DiscardHandler), postgresstore.NewQueriesWithPool(e.pool, dbsqlc.New(e.pool)))
	rowIDs := make([]string, len(inputs))
	for i, in := range inputs {
		modelMessage := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.UserMessage(in.text)})[0]
		raw, err := json.Marshal(modelMessage)
		if err != nil {
			t.Fatal(err)
		}
		meta := map[string]any{"route_id": e.routeID, "platform": discussTestPlatform}
		row, err := svc.Persist(ctx, messagepkg.PersistInput{
			BotID: e.botID, SessionID: e.sessionID, Role: "user", Content: raw,
			SenderChannelIdentityID: in.from.identityID, ExternalMessageID: in.externalID,
			DisplayText: in.text, Metadata: meta, Admission: in.refused, Assets: in.assets,
		})
		if err != nil {
			t.Fatalf("persist %s: %v", in.externalID, err)
		}
		rowIDs[i] = row.ID
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range inputs {
		if in.refused != "" {
			continue
		}
		eventData, err := json.Marshal(map[string]any{"message_id": in.externalID, "mentions_me": in.mentionsMe})
		if err != nil {
			t.Fatal(err)
		}
		eventID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO bot_session_events (id, bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms, mentions_me, replies_to_me)
			VALUES ($1, $2, $3, 'message', $4, $5, $6, $7, $8)
		`, eventID, e.botID, e.sessionID, eventData, in.externalID, time.Now().UnixMilli(), in.mentionsMe, in.repliesToMe); err != nil {
			t.Fatalf("insert event %s: %v", in.externalID, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE bot_history_messages SET event_id = $2 WHERE id = $1`, rowIDs[i], eventID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	positions := make([]int64, len(inputs))
	for i := range inputs {
		if err := e.pool.QueryRow(ctx, `SELECT turn_position FROM bot_history_messages WHERE id = $1`, rowIDs[i]).Scan(&positions[i]); err != nil {
			t.Fatal(err)
		}
	}
	return positions
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
		SELECT invocation_id, run_id::text, state, COALESCE(discuss_cover_position, 0), input_json
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

func (e discussEnv) invocation(hw int64) string { return pending.DiscussInvocationID(e.sessionID, hw) }

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
		SessionID: db.ParseUUIDOrEmpty(e.sessionID), States: pending.CoverStates,
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

// discussSetup is what newDiscussProcessWith builds a process from; an option
// changes it before the service is assembled.
type discussSetup struct {
	manager *sessionruntime.Manager
	fixture []directLifecycleOption
	// compatibilities are what the run's model declares it can do.
	compatibilities []string
	contextWindow   int
	// observe sees the run configuration as the agent receives it, before the
	// provider context is built from it.
	observe func(native.RunConfig)
}

type discussOption func(*discussSetup)

// discussFixture adds options of the chat fixture underneath the process.
func discussFixture(options ...directLifecycleOption) discussOption {
	return func(s *discussSetup) { s.fixture = append(s.fixture, options...) }
}

// discussService adds newTestService options to the process's service.
func discussService(opts ...testOption) discussOption {
	return discussFixture(withFixtureOptions(opts...))
}

// discussDeps changes the Deps of the process's service.
func discussDeps(f func(*Deps)) discussOption {
	return discussFixture(withFixtureDeps(f))
}

// discussTeam serves the process's service for one team only.
func discussTeam(teamID string) discussOption {
	return discussService(withAllowedTeam(teamID))
}

// discussObserveConfig reports the configuration of each run to observe.
func discussObserveConfig(observe func(native.RunConfig)) discussOption {
	return func(s *discussSetup) { s.observe = observe }
}

func newDiscussProcess(t *testing.T, env discussEnv, steps ...*discussStep) *discussProcess {
	t.Helper()
	return newDiscussProcessWith(t, env, nil, steps...)
}

func newDiscussProcessWith(t *testing.T, env discussEnv, options []discussOption, steps ...*discussStep) *discussProcess {
	t.Helper()
	pg := postgresstore.NewQueriesWithPool(env.pool, dbsqlc.New(env.pool))
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-discuss-" + uuid.NewString(),
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Minute,
		Ledger:        ledger.NewPostgres(dbsqlc.New(env.pool), env.pool),
		Fence:         runtimefence.NewActivator(pg),
	})
	t.Cleanup(func() { _ = manager.Close() })
	setup := discussSetup{manager: manager, compatibilities: []string{"tool-call"}, contextWindow: 128000}
	for _, option := range options {
		option(&setup)
	}

	provider := &discussProvider{steps: steps}
	sender := &discussSender{}
	logger := slog.New(slog.DiscardHandler)
	applier := contextview.ProviderRunConfigApplier(logger)
	agent := native.New(native.Deps{
		Logger: logger,
		ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) native.RunConfig {
			if setup.observe != nil {
				setup.observe(cfg)
			}
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
		tools.NewMessageProvider(nil, sender, agenttest.MessageReactor{}, agenttest.ChannelParser{}, nil),
	})
	base := []directLifecycleOption{
		withFixtureRuntime(manager),
		withFixtureMessages(messagepkg.NewService(logger, pg)),
		withFixtureQueries(func(q dbstore.Queries) dbstore.Queries { return discussStore{Queries: q, pg: pg} }),
		withFixtureDeps(func(d *Deps) {
			d.Agent = agent
			d.RunCredentialSecret = discussTestSecret
			d.Sessions = &agenttest.Sessions{
				GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{
						ID: id, BotID: env.botID, Type: sessionpkg.TypeDiscuss,
						SessionMode: sessionpkg.TypeDiscuss, RuntimeType: sessionpkg.RuntimeModel,
						Facts: sessionfacts.Preset(sessionpkg.TypeDiscuss),
					}, nil
				},
				UpdateMetadataFn: func(_ context.Context, id string, _ map[string]any) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{ID: id}, nil
				},
			}
		}),
	}
	chat := newChatTurnFixture(t, directLifecycleModelSuccess, append(base, setup.fixture...)...)
	// The run offers tools only to a model that can call them.
	for key, model := range chat.fixture.queries.Models {
		config, err := json.Marshal(map[string]any{"context_window": setup.contextWindow, "compatibilities": setup.compatibilities})
		if err != nil {
			t.Fatal(err)
		}
		model.Config = config
		chat.fixture.queries.Models[key] = model
	}
	return &discussProcess{svc: chat.fixture.service, manager: manager, provider: provider, sender: sender}
}

func (p *discussProcess) admit(t *testing.T, env discussEnv) (pending.DiscussAdmission, <-chan struct{}) {
	t.Helper()
	outcome, done, err := p.svc.pending.AdmitDiscussRun(context.Background(), env.botID, env.sessionID)
	if err != nil {
		t.Fatalf("admitDiscuss() error = %v", err)
	}
	return outcome, done
}

// answer makes sure the batch ending at hw is answered and waits for its run
// to complete. A run's end checks the session again by itself, so input
// written after a run may already have been admitted by then; the batch is
// answered either way, by that admission or by this one.
func (p *discussProcess) answer(t *testing.T, env discussEnv, hw int64) {
	t.Helper()
	p.answerFrom(t, "", env, hw)
}

// answerFrom is answer with the admissions made on behalf of the request with
// the given id.
func (p *discussProcess) answerFrom(t *testing.T, requestID string, env discussEnv, hw int64) {
	t.Helper()
	admit := func() (<-chan struct{}, error) {
		ctx := logger.ContextWithRequestID(context.Background(), requestID)
		_, done, err := p.svc.pending.AdmitDiscussRun(ctx, env.botID, env.sessionID)
		return done, err
	}
	env.awaitAnswered(t, hw, admit)
}

// awaitAnswered admits until the batch ending at hw has been answered.
func (e discussEnv) awaitAnswered(t *testing.T, hw int64, admit func() (<-chan struct{}, error)) {
	t.Helper()
	for attempt := 0; attempt < 1500; attempt++ {
		done, err := admit()
		if err != nil {
			t.Fatalf("admitDiscuss() error = %v", err)
		}
		if done != nil {
			waitDiscussRun(t, done)
		}
		if e.answered(t, hw) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the batch ending at %d was not answered", hw)
}

// answered reports whether the run for the batch ending at hw has completed.
func (e discussEnv) answered(t *testing.T, hw int64) bool {
	t.Helper()
	run, ok := e.runs(t)[e.invocation(hw)]
	return ok && run.state == "completed"
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

// titleModelAccountStore answers the account lookup with one fixed record.
type titleModelAccountStore struct {
	dbstore.AccountStore
	account dbstore.AccountRecord
}

func (s titleModelAccountStore) GetByUserID(context.Context, string) (dbstore.AccountRecord, error) {
	return s.account, nil
}

// pagedScopes names its scopes one to a page.
type pagedScopes struct {
	scopes []string
	mu     sync.Mutex
	bound  []string
}

type pagedScopeKey struct{}

func (*pagedScopes) CurrentScope(ctx context.Context) string {
	scope, _ := ctx.Value(pagedScopeKey{}).(string)
	return scope
}

func (p *pagedScopes) ListScopePage(_ context.Context, after string, _ int32) (ports.TeamScopePage, error) {
	next := 0
	if after != "" {
		n, err := strconv.Atoi(after)
		if err != nil {
			return ports.TeamScopePage{}, err
		}
		next = n
	}
	page := ports.TeamScopePage{Scopes: []string{p.scopes[next]}}
	if next+1 >= len(p.scopes) {
		page.Complete = true
		return page, nil
	}
	page.NextCursor = strconv.Itoa(next + 1)
	return page, nil
}

func (p *pagedScopes) BindScope(ctx context.Context, scope string) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound = append(p.bound, scope)
	return context.WithValue(ctx, pagedScopeKey{}, scope)
}

func (p *pagedScopes) visited() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bound...)
}

type runTurn struct {
	turnID   string
	position int64
}

func (e discussEnv) runTurn(t *testing.T, invocation string) runTurn {
	t.Helper()
	var rt runTurn
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT turn_id::text, turn_position FROM session_runs WHERE session_id = $1 AND invocation_id = $2`,
		e.sessionID, invocation).Scan(&rt.turnID, &rt.position))
	return rt
}

func (e discussEnv) counter(t *testing.T) int64 {
	t.Helper()
	var next int64
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT next_turn_position FROM bot_sessions WHERE id = $1`, e.sessionID).Scan(&next))
	return next
}

func (e discussEnv) rowsOfRun(t *testing.T, invocation string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT m.turn_id::text FROM bot_history_messages m
		JOIN session_runs r ON r.run_id = m.run_id
		WHERE r.session_id = $1 AND r.invocation_id = $2`, e.sessionID, invocation)
	require.NoError(t, err)
	defer rows.Close()
	var turns []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		turns = append(turns, id)
	}
	require.NoError(t, rows.Err())
	return turns
}

// writeHeaded stores a message the way the inbound path records one nobody is
// answering yet: a user row with its header, and the session event that makes it
// a message the discuss run answers.
func (e discussEnv) writeHeaded(t *testing.T, from discussParticipant, externalID, text, replyTo string, mentionsMe bool, at time.Time, attachments []turn.Attachment) {
	t.Helper()
	ctx := context.Background()
	var paths []string
	for _, att := range attachments {
		if path := strings.TrimSpace(att.Path); path != "" {
			paths = append(paths, path)
		}
	}
	header := turn.FormatUserHeader(turn.UserMessageHeaderInput{
		MessageID:         externalID,
		ChannelIdentityID: from.identityID,
		DisplayName:       "participant",
		Channel:           discussTestPlatform,
		ConversationType:  "private",
		Target:            discussTestTarget,
		AttachmentPaths:   paths,
		Time:              at,
		ReplyToMessageID:  replyTo,
		MentionsMe:        mentionsMe,
	}, text)
	raw, err := json.Marshal(turn.ModelMessage{Role: "user", Content: turn.NewTextContent(header)})
	require.NoError(t, err)
	meta := map[string]any{"route_id": e.routeID, "platform": discussTestPlatform}
	if replyTo != "" {
		meta["reply"] = map[string]any{"message_id": replyTo}
	}
	svc := messagepkg.NewService(slog.New(slog.DiscardHandler), postgresstore.NewQueriesWithPool(e.pool, dbsqlc.New(e.pool)))
	row, err := svc.Persist(ctx, messagepkg.PersistInput{
		BotID: e.botID, SessionID: e.sessionID, Role: "user", Content: raw,
		SenderChannelIdentityID: from.identityID, ExternalMessageID: externalID,
		SourceReplyToMessageID: replyTo, DisplayText: text, Metadata: meta,
		Assets: assetRefsOf(attachments),
	})
	require.NoError(t, err)
	eventData, err := json.Marshal(map[string]any{"message_id": externalID, "mentions_me": mentionsMe})
	require.NoError(t, err)
	eventID := uuid.New()
	_, err = e.pool.Exec(ctx, `
		INSERT INTO bot_session_events (id, bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms, mentions_me)
		VALUES ($1, $2, $3, 'message', $4, $5, $6, $7)
	`, eventID, e.botID, e.sessionID, eventData, externalID, at.UnixMilli(), mentionsMe)
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE bot_history_messages SET event_id = $2 WHERE id = $1`, row.ID, eventID)
	require.NoError(t, err)
}
