package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/compact"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

// pgContextMessages reads a session's context from the real PostgreSQL store
// under the session id the fixture's run carries; everything else is the
// fixture's in-memory service.
type pgContextMessages struct {
	agenttest.RecordingMessages
	db        *messagepkg.DBService
	sessionID string
}

func (m *pgContextMessages) ListActiveAfterCompactionBySessionWithinBytes(ctx context.Context, _ string, maxBytes int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	return m.db.ListActiveAfterCompactionBySessionWithinBytes(ctx, m.sessionID, maxBytes)
}

func (m *pgContextMessages) ListRecentUserBySession(ctx context.Context, _ string, limit int) ([]messagepkg.Message, error) {
	return m.db.ListRecentUserBySession(ctx, m.sessionID, limit)
}

type imGroupMember struct {
	identityID string
	name       string
}

var (
	imAlice = imGroupMember{"identity-alice", "Alice"}
	imBob   = imGroupMember{"identity-bob", "Bob"}
	imCarol = imGroupMember{"identity-carol", "Carol"}
)

// imGroupHeader renders a group member's message the way the inbound path
// writes it, for a message nobody asked the bot to answer and for one that
// started a run.
func imGroupHeader(member imGroupMember, messageID, text string, at time.Time, mutate func(*turn.UserMessageHeaderInput)) string {
	input := turn.UserMessageHeaderInput{
		MessageID:         messageID,
		ChannelIdentityID: member.identityID,
		DisplayName:       member.name,
		Channel:           "telegram",
		ConversationType:  "group",
		ConversationName:  "Release crew",
		Target:            "im-group-1",
		Time:              at,
	}
	if mutate != nil {
		mutate(&input)
	}
	return turn.FormatUserHeader(input, text)
}

// These goldens pin what the model provider receives when a run starts from
// each entry and travels the whole application path to the provider call. The
// recorded request is the contract; how a test reaches each entry is glue that
// follows the entry functions as they move. The run-requests goldens cover the
// other axis: one session kind handed straight to the native runtime.

var updateDriveRequests = flag.Bool("update-drive-requests", false, "rewrite testdata/drive-requests golden files")

// driveRequestRecorder is the provider every run in these tests talks to. It
// keeps each request the model is asked to stream and answers it with a short
// completed reply, so a run reaches the provider and ends cleanly.
type driveRequestRecorder struct {
	mu       sync.Mutex
	requests []sdk.Request
}

func (*driveRequestRecorder) Name() string { return "drive-request-recorder" }

func (*driveRequestRecorder) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*driveRequestRecorder) Test(context.Context) error { return nil }

func (*driveRequestRecorder) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (p *driveRequestRecorder) DoGenerate(_ context.Context, req sdk.Request) (sdk.ModelResult, error) {
	p.record(req)
	return sdk.ModelResult{Text: directLifecycleResponse, FinishReason: sdk.FinishReasonStop}, nil
}

func (p *driveRequestRecorder) DoStream(_ context.Context, req sdk.Request) (<-chan sdk.StreamPart, error) {
	p.record(req)
	ch := make(chan sdk.StreamPart, 8)
	ch <- &sdk.StartPart{}
	ch <- &sdk.StartStepPart{}
	ch <- &sdk.TextStartPart{ID: "answer"}
	ch <- &sdk.TextDeltaPart{ID: "answer", Text: directLifecycleResponse}
	ch <- &sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop}
	ch <- &sdk.FinishPart{FinishReason: sdk.FinishReasonStop}
	close(ch)
	return ch, nil
}

func (p *driveRequestRecorder) record(req sdk.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
}

func (p *driveRequestRecorder) recorded() []sdk.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sdk.Request(nil), p.requests...)
}

// newDriveRequestFixture is the direct lifecycle service with its model
// provider swapped for the recorder. The swap happens in the context view
// applier, after the application has assembled the run config and the
// production applier has compiled the provider-facing fields, so every byte
// the recorder sees came from the real assembly.
//
// The agent carries the message and ask_user tools, and the service a user
// input port, so the request shows the run's flags: ask_user is offered only
// to a run that can deliver a question, and the message tool describes where
// a reply goes.
func newDriveRequestFixture(t *testing.T, options ...directLifecycleOption) (directLifecycleFixture, *driveRequestRecorder) {
	t.Helper()
	recorder := &driveRequestRecorder{}
	logger := slog.New(slog.DiscardHandler)
	applier := contextview.ProviderRunConfigApplier(logger)
	agent := native.New(native.Deps{
		Logger: logger,
		ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) native.RunConfig {
			out := applier(ctx, cfg)
			if out.Model != nil {
				model := *out.Model
				model.Provider = recorder
				out.Model = &model
			}
			return out
		},
	})
	agent.SetToolProviders([]tools.ToolProvider{
		tools.NewMessageProvider(nil, agenttest.MessageSender{}, agenttest.MessageReactor{}, agenttest.ChannelParser{}, nil),
		tools.NewAskUserProvider(nil),
	})
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess,
		append([]directLifecycleOption{withFixtureDeps(func(d *Deps) { d.Agent = agent })}, options...)...)
	// The run offers tools only to a model that can call them.
	queries := fixture.queries
	for key, model := range queries.Models {
		model.Config = []byte(`{"context_window":128000,"compatibilities":["tool-call"]}`)
		queries.Models[key] = model
	}
	return fixture, recorder
}

// driveRequestRuntime gives the fixture a real session runtime over an
// in-memory ledger, for the entries that publish to it or need its fence.
func driveRequestRuntime(t *testing.T) directLifecycleOption {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-drive-request-golden",
		OwnerLeaseTTL: time.Minute,
		Ledger:        sessionledger.New(),
		Fence:         agenttest.NoopFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	return withFixtureRuntime(manager)
}

// driveRequestHeaderTime matches the wall-clock time the user message header
// carries; it is the one field of a request that changes between runs.
var driveRequestHeaderTime = regexp.MustCompile(`t=\\"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})\\"`)

// marshalDriveRequests renders the requests one run sent, with the header time
// replaced by a fixed value.
func marshalDriveRequests(t *testing.T, requests []sdk.Request) []byte {
	t.Helper()
	got, err := json.MarshalIndent(requests, "", "  ")
	if err != nil {
		t.Fatalf("marshal requests: %v", err)
	}
	got = driveRequestHeaderTime.ReplaceAll(got, []byte(`t=\"<time>\"`))
	return append(got, '\n')
}

// assertDriveGolden compares got with testdata/<dir>/<name>.json, or rewrites
// it under -update-drive-requests.
func assertDriveGolden(t *testing.T, dir, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", dir, name+".json")
	if *updateDriveRequests {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Fatalf("read golden: %v (run with -update-drive-requests to create)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("what the %s entry sent differs from %s:\n%s", name, path, got)
	}
}

// requireDriveRequests returns the requests one run made, failing the test
// when the run never reached the provider.
func requireDriveRequests(t *testing.T, recorder *driveRequestRecorder) []sdk.Request {
	t.Helper()
	requests := recorder.recorded()
	if len(requests) == 0 {
		t.Fatal("the run did not reach the model provider")
	}
	return requests
}

const (
	driveEarlierUserMessageID      = "a1111111-1111-4111-8111-111111111111"
	driveEarlierAssistantMessageID = "a2222222-2222-4222-8222-222222222222"
	driveRequestMessageID          = "a3333333-3333-4333-8333-333333333333"
	driveReplacedAssistantID       = "a4444444-4444-4444-8444-444444444444"
	driveReplacedTurnID            = "a5555555-5555-4555-8555-555555555555"
	driveToolCallID                = "drive-tool-call"
	driveTeamID                    = "a6666666-6666-4666-8666-666666666666"
	driveInterruptedRunID          = "a7777777-7777-4777-8777-777777777777"
)

// driveHistoryMessages serves a fixed session history: an earlier exchange
// followed by the latest turn, whose request and answer a retry or edit
// replaces, or whose tool call a decision continuation answers.
type driveHistoryMessages struct {
	agenttest.RecordingMessages
	history []messagepkg.Message
	latest  messagepkg.HistoryTurn
}

func (m *driveHistoryMessages) ListActiveAfterCompactionBySessionWithinBytes(context.Context, string, int64) (messagepkg.SessionHistoryAfterCompaction, error) {
	return messagepkg.SessionHistoryAfterCompaction{Messages: append([]messagepkg.Message(nil), m.history...)}, nil
}

func (m *driveHistoryMessages) GetLatestVisibleTurnBySession(context.Context, string) (messagepkg.HistoryTurn, error) {
	return m.latest, nil
}

func (m *driveHistoryMessages) GetByIDBySession(_ context.Context, _ string, messageID string) (messagepkg.Message, error) {
	for _, message := range m.history {
		if message.ID == messageID {
			return message, nil
		}
	}
	return messagepkg.Message{}, errors.New("message not found")
}

// driveReplaceableHistory is a session whose latest turn asked the prompt and
// got an answer a retry or edit replaces.
func driveReplaceableHistory(t *testing.T) *driveHistoryMessages {
	t.Helper()
	return &driveHistoryMessages{
		history: []messagepkg.Message{
			agenttest.AdmissionHistoryMessage(t, driveEarlierUserMessageID, sdk.UserMessage("an earlier question"), nil),
			agenttest.AdmissionHistoryMessage(t, driveEarlierAssistantMessageID, sdk.AssistantMessage("an earlier answer"), nil),
			agenttest.AdmissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
			agenttest.AdmissionHistoryMessage(t, driveReplacedAssistantID, sdk.AssistantMessage("the answer being replaced"), nil),
		},
		latest: messagepkg.HistoryTurn{
			ID:                 driveReplacedTurnID,
			RequestMessageID:   driveRequestMessageID,
			AssistantMessageID: driveReplacedAssistantID,
		},
	}
}

// driveDecisionHistory is a session whose latest turn called toolName and has
// the answer to it stored, which is what a decision continuation resumes from.
func driveDecisionHistory(t *testing.T, toolName string, result sdk.ToolOutput) *driveHistoryMessages {
	t.Helper()
	call := sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{sdk.ToolCallPart{
		ToolCallID: driveToolCallID,
		ToolName:   toolName,
		Input:      sdk.ParseToolArguments(`{}`),
	}}}
	return &driveHistoryMessages{RecordingMessages: agenttest.RecordingMessages{RunRequest: &messagepkg.RunTurnRequest{MessageID: driveRequestMessageID}}, history: []messagepkg.Message{
		agenttest.AdmissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
		agenttest.AdmissionHistoryMessage(t, driveReplacedAssistantID, call, nil),
		agenttest.AdmissionHistoryMessage(t, driveEarlierAssistantMessageID, sdk.ToolMessage(sdk.ToolResultPart{
			ToolCallID: driveToolCallID,
			ToolName:   toolName,
			Result:     result,
		}), nil),
	}}
}

// driveSchedulePayload is a fire as the schedule service sends it: the
// schedule's own model for the built-in agent, a separate model for an
// External Agent runtime, and its reasoning effort.
func driveSchedulePayload(ownerUserID string) schedule.TriggerPayload {
	maxCalls := 3
	return schedule.TriggerPayload{
		ID:              "schedule-1",
		FireID:          "fire-1",
		Name:            "daily digest",
		Description:     "summarize the day",
		Pattern:         "0 9 * * *",
		MaxCalls:        &maxCalls,
		Command:         directLifecyclePrompt,
		SessionID:       agenttest.LifecycleSessionID,
		OwnerUserID:     ownerUserID,
		ModelID:         "direct-lifecycle-model",
		RuntimeModelID:  "external-runtime-model",
		ReasoningEffort: "medium",
	}
}

// drivePromptRecorder is an External Agent runtime that keeps every prompt it
// is given and completes each turn with a short answer.
type drivePromptRecorder struct {
	mu     sync.Mutex
	inputs []external.PromptInput
}

func (*drivePromptRecorder) RuntimeType() string { return session.RuntimeACPAgent }

func (d *drivePromptRecorder) Prompt(_ context.Context, input external.PromptInput) (external.PromptResult, error) {
	d.mu.Lock()
	d.inputs = append(d.inputs, input)
	d.mu.Unlock()
	return external.PromptResult{
		Text:          directLifecycleResponse,
		Output:        []sdk.Message{sdk.AssistantMessage(directLifecycleResponse)},
		StopReason:    "end_turn",
		TurnCompleted: true,
	}, nil
}

func (d *drivePromptRecorder) recorded(t *testing.T) []external.PromptInput {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.inputs) == 0 {
		t.Fatal("the run did not reach the External Agent runtime")
	}
	return append([]external.PromptInput(nil), d.inputs...)
}

// newDrivePromptFixture is a service whose session runs on an External Agent
// runtime, with the recorder registered as that runtime's driver and an
// admitted run for the terminal write. The user input port lets the prompt
// show whether the run can deliver a question.
func newDrivePromptFixture(t *testing.T, opts ...testOption) (*Service, *drivePromptRecorder) {
	t.Helper()
	recorder := &drivePromptRecorder{}
	service := newACPLifecycleService(t, &recordingACPPrompter{}, &agenttest.RecordingMessages{}, &agenttest.RecordingLifecycleStore{},
		append([]testOption{withDecide(func(d *decide.Deps) { d.UserInput = &agenttest.UserInput{} }), withDrivers(recorder)}, opts...)...)
	return service, recorder
}

// memoryQueuedInputs stands in for the queue table where a case only needs the
// drain to be handed a batch. What the table guarantees, the claim, the lease
// and the order, is tested against PostgreSQL; a case that depends on it uses
// the real queue.
type memoryQueuedInputs struct {
	pending.Store

	mu       sync.Mutex
	items    []sessionruntime.QueuedInput
	settled  []string
	consumed []string
}

func (m *memoryQueuedInputs) Enqueue(_ context.Context, in sessionruntime.QueueEnqueue) (sessionruntime.QueuedInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := sessionruntime.QueuedInput{
		ID: "queued-" + strconv.Itoa(len(m.items)+1), BotID: in.Key.BotID, SessionID: in.Key.SessionID,
		Origin: in.Origin, InvocationID: in.InvocationID, MergeKey: in.MergeKey, Payload: in.Payload,
		Position: int64(len(m.items) + 1), State: sessionruntime.QueuedStateQueued,
	}
	m.items = append(m.items, item)
	return item, nil
}

func (*memoryQueuedInputs) Gate(context.Context, sessionruntime.Key, string) (sessionruntime.QueueGate, error) {
	return sessionruntime.QueueGate{}, nil
}

// ClaimBatch hands out every queued input of the session as one batch.
func (m *memoryQueuedInputs) ClaimBatch(_ context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin) (sessionruntime.QueueBatch, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	batch := sessionruntime.QueueBatch{ID: "batch-" + strconv.Itoa(len(m.settled)+1), Key: key, Origin: origin}
	for i := range m.items {
		if m.items[i].State == sessionruntime.QueuedStateQueued && m.items[i].SessionID == key.SessionID && m.items[i].Origin == origin {
			m.items[i].State = sessionruntime.QueuedStateClaimed
			batch.Items = append(batch.Items, m.items[i])
		}
	}
	return batch, len(batch.Items) > 0, nil
}

func (m *memoryQueuedInputs) SettleBatch(_ context.Context, batch sessionruntime.QueueBatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled = append(m.settled, batch.ID)
	for i := range m.items {
		if m.items[i].State == sessionruntime.QueuedStateClaimed {
			m.items[i].State = sessionruntime.QueuedStateConsumed
		}
	}
	return nil
}

func (m *memoryQueuedInputs) Pending(_ context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin) ([]sessionruntime.QueuedInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pending []sessionruntime.QueuedInput
	for _, item := range m.items {
		if item.State == sessionruntime.QueuedStateQueued && item.SessionID == key.SessionID && item.Origin == origin {
			pending = append(pending, item)
		}
	}
	return pending, nil
}

func (m *memoryQueuedInputs) ConsumePromoted(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consumed = append(m.consumed, id)
	return nil
}

// compactionFixedSession creates the bot and session the drive fixture's turn
// runs in (the lifecycle test ids), so the real compaction service and the
// run read and write the same rows.
func compactionFixedSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	userID := uuid.New()
	name := "pre-turn-compaction-" + uuid.NewString()
	cleanup := context.WithoutCancel(ctx)
	_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", agenttest.LifecycleBotID)
	if _, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, agenttest.LifecycleBotID, userID, name); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')`, agenttest.LifecycleSessionID, agenttest.LifecycleBotID); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", agenttest.LifecycleBotID)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})
}

// compactionQueries answers the compaction artifact reads from the database; the
// fixture's own fake answers them with an empty frontier.
type compactionQueries struct {
	noDecisionQueries
	real dbstore.Queries
}

func (q compactionQueries) ListCompactionArtifactLineageBySession(ctx context.Context, id pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return q.real.ListCompactionArtifactLineageBySession(ctx, id)
}

func (q compactionQueries) GetCompactionLogByID(ctx context.Context, id pgtype.UUID) (sqlc.BotHistoryMessageCompact, error) {
	return q.real.GetCompactionLogByID(ctx, id)
}

func (q compactionQueries) ListCompactionArtifactParentIDsBySuccessor(ctx context.Context, arg sqlc.ListCompactionArtifactParentIDsBySuccessorParams) ([]pgtype.UUID, error) {
	return q.real.ListCompactionArtifactParentIDsBySuccessor(ctx, arg)
}

func (q compactionQueries) ListMessageRefsByCompactID(ctx context.Context, id pgtype.UUID) ([]sqlc.ListMessageRefsByCompactIDRow, error) {
	return q.real.ListMessageRefsByCompactID(ctx, id)
}

// compactionDriveSession is a drive-request fixture whose history is real
// PostgreSQL rows, whose compaction is the real compaction service, and whose
// model window is small enough that ten rows pass the blocking share of it.
func compactionDriveSession(t *testing.T) (*agenttest.CompactionSession, directLifecycleFixture, *driveRequestRecorder) {
	t.Helper()
	ctx := context.Background()
	pool := agenttest.OpenPostgres(t, ctx)
	compactionFixedSession(t, ctx, pool)
	s := agenttest.NewCompactionSessionFor(t, ctx, pool, agenttest.LifecycleBotID, agenttest.LifecycleSessionID)

	fixture, recorder := newDriveRequestFixture(t,
		driveRequestRuntime(t),
		withFixtureMessages(&pgContextMessages{db: s.Messages, sessionID: s.SessionID}),
		withFixtureQueries(func(q dbstore.Queries) dbstore.Queries {
			return compactionQueries{noDecisionQueries: q.(noDecisionQueries), real: s.Queries}
		}),
		withFixtureOptions(withCompact(func(d *compact.Deps) {
			d.Runner = withoutModelRecord{s.Compaction}
			d.HTTPClient = &http.Client{Transport: s.Summarizer}
		})))
	queries := fixture.queries
	// Everything the fixture does not fake, the compaction frontier included,
	// is answered by the database.
	queries.Queries = s.Queries
	for key, model := range queries.Models {
		model.Config = []byte(`{"context_window":60000,"compatibilities":["tool-call"]}`)
		queries.Models[key] = model
	}
	return s, fixture, recorder
}

// withoutModelRecord drops the summarizer's model record id: the fixture's
// model exists only in memory, and the artifact row's foreign key to the
// models table would refuse it.
type withoutModelRecord struct{ inner *compaction.Service }

func (w withoutModelRecord) RunCompactionSync(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	cfg.ModelRecordID = ""
	return w.inner.RunCompactionSync(ctx, cfg)
}

func startCompactionTurn(t *testing.T, fixture directLifecycleFixture, query string) {
	t.Helper()
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: "drive-request-team", Mode: turn.ModeChat,
		BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID, ThreadID: agenttest.LifecycleSessionID,
		Query: query, UserVisibleText: query,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainTurn(t, handle)
}
