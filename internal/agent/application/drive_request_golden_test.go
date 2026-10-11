package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/contextview"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/testutil/sessionledger"
)

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
func newDriveRequestFixture(t *testing.T) (directLifecycleFixture, *driveRequestRecorder) {
	t.Helper()
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)
	recorder := &driveRequestRecorder{}
	logger := slog.New(slog.DiscardHandler)
	applier := contextview.ProviderRunConfigApplier(logger)
	agent := native.New(native.Deps{
		Logger: logger,
		ContextViewApplier: func(ctx context.Context, cfg native.RunConfig) (native.RunConfig, error) {
			out, err := applier(ctx, cfg)
			if out.Model != nil {
				model := *out.Model
				model.Provider = recorder
				out.Model = &model
			}
			return out, err
		},
	})
	agent.SetToolProviders([]tools.ToolProvider{
		tools.NewMessageProvider(nil, goldenMessagingSender{}, goldenMessagingReactor{}, goldenChannelResolver{}, nil),
		tools.NewAskUserProvider(nil),
	})
	fixture.service.agent = agent
	fixture.service.userInput = &fakeUserInputService{}
	// The run offers tools only to a model that can call them.
	queries := fixture.service.queries.(*directLifecycleQueries)
	for key, model := range queries.models {
		model.Config = []byte(`{"context_window":128000,"compatibilities":["tool-call"]}`)
		queries.models[key] = model
	}
	return fixture, recorder
}

// withDriveRequestRuntime gives the fixture a real session runtime over an
// in-memory ledger, for the entries that publish to it or need its fence.
func withDriveRequestRuntime(t *testing.T, fixture directLifecycleFixture) *sessionruntime.Manager {
	t.Helper()
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-drive-request-golden",
		OwnerLeaseTTL: time.Minute,
		Ledger:        sessionledger.New(),
		Fence:         abortAlignmentFence{},
	})
	t.Cleanup(func() { _ = manager.Close() })
	fixture.service.SetSessionRuntime(manager)
	// The real manager asks for the run's pending decisions when it finishes.
	fixture.service.queries = noDecisionQueries{fixture.service.queries.(*directLifecycleQueries)}
	return manager
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

// drainDriveHandle consumes a turn handle to its end.
func drainDriveHandle(t *testing.T, handle turn.RunHandle) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range handle.Events() {
		}
		for range handle.Errs() {
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn did not finish")
	}
}

// driveWebSend runs a Web chat send the way the WebSocket handler hands it to
// the application.
func driveWebSend(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	eventCh := make(chan WSStreamEvent)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range eventCh {
		}
	}()
	_, err := fixture.service.StreamChatWS(context.Background(), ChatRequest{
		BotID:           lifecycleTestBotID,
		ChatID:          lifecycleTestBotID,
		ThreadID:        lifecycleTestSessionID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
		CurrentChannel:  "local",
	}, eventCh, make(chan struct{}))
	close(eventCh)
	<-drained
	if err != nil {
		t.Fatalf("StreamChatWS() error = %v", err)
	}
	return requireDriveRequests(t, recorder)
}

// driveIMStartTurn runs an inbound IM message through the turn port.
func driveIMStartTurn(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	withDriveRequestRuntime(t, fixture)
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "drive-request-team",
		Mode:                    turn.ModeChat,
		BotID:                   lifecycleTestBotID,
		ChatID:                  lifecycleTestBotID,
		ThreadID:                lifecycleTestSessionID,
		Query:                   directLifecyclePrompt,
		UserVisibleText:         directLifecyclePrompt,
		ExternalMessageID:       "im-message-1",
		SourceChannelIdentityID: "im-sender-1",
		DisplayName:             "IM Sender",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-chat-1",
		ConversationType:        "private",
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainDriveHandle(t, handle)
	return requireDriveRequests(t, recorder)
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
	recordingMessageService
	history []messagepkg.Message
	latest  messagepkg.HistoryTurn
}

func (m *driveHistoryMessages) ListActiveSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error) {
	return append([]messagepkg.Message(nil), m.history...), nil
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
			admissionHistoryMessage(t, driveEarlierUserMessageID, sdk.UserMessage("an earlier question"), nil),
			admissionHistoryMessage(t, driveEarlierAssistantMessageID, sdk.AssistantMessage("an earlier answer"), nil),
			admissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
			admissionHistoryMessage(t, driveReplacedAssistantID, sdk.AssistantMessage("the answer being replaced"), nil),
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
	return &driveHistoryMessages{history: []messagepkg.Message{
		admissionHistoryMessage(t, driveRequestMessageID, sdk.UserMessage(directLifecyclePrompt), nil),
		admissionHistoryMessage(t, driveReplacedAssistantID, call, nil),
		admissionHistoryMessage(t, driveEarlierAssistantMessageID, sdk.ToolMessage(sdk.ToolResultPart{
			ToolCallID: driveToolCallID,
			ToolName:   toolName,
			Result:     result,
		}), nil),
	}}
}

// runWebReplacement runs a Web retry or edit and drains what it delivers.
func runWebReplacement(t *testing.T, run func(chan<- WSStreamEvent) error) {
	t.Helper()
	eventCh := make(chan WSStreamEvent)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range eventCh {
		}
	}()
	err := run(eventCh)
	close(eventCh)
	<-drained
	if err != nil {
		t.Fatalf("replacement run error = %v", err)
	}
}

// driveWebRetry regenerates the latest answer from the stored request.
func driveWebRetry(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	fixture.service.messageService = driveReplaceableHistory(t)
	runWebReplacement(t, func(eventCh chan<- WSStreamEvent) error {
		_, err := fixture.service.RetryLatestMessageWS(context.Background(), RetryLatestMessageInput{
			BotID:        lifecycleTestBotID,
			SessionID:    lifecycleTestSessionID,
			TargetTurnID: driveReplacedTurnID,
			RunID:        lifecycleTestRunID,
		}, eventCh, make(chan struct{}))
		return err
	})
	return requireDriveRequests(t, recorder)
}

// driveWebEdit replaces the latest request with new text.
func driveWebEdit(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	fixture.service.messageService = driveReplaceableHistory(t)
	runWebReplacement(t, func(eventCh chan<- WSStreamEvent) error {
		_, err := fixture.service.EditLatestMessageWS(context.Background(), EditLatestMessageInput{
			BotID:        lifecycleTestBotID,
			SessionID:    lifecycleTestSessionID,
			TargetTurnID: driveReplacedTurnID,
			RunID:        lifecycleTestRunID,
			Text:         "the edited question",
		}, eventCh, make(chan struct{}))
		return err
	})
	return requireDriveRequests(t, recorder)
}

// awaitDriveLifecycle waits for the run's context lifecycle terminal, which
// is written once the run has ended, for entries that drain their own run.
func awaitDriveLifecycle(t *testing.T, fixture directLifecycleFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(fixture.lifecycles.creates()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// driveFollowUp queues an IM message while the session is busy and starts it
// from the queue once the running turn ends.
func driveFollowUp(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	backend := sessionruntime.NewMemoryBackend()
	key := sessionruntime.Key{BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID}
	setRun := func(status string) {
		t.Helper()
		if _, _, err := backend.Update(context.Background(), key, func(snapshot sessionruntime.Snapshot, _ bool) (sessionruntime.Snapshot, bool, error) {
			snapshot.BotID, snapshot.SessionID = key.BotID, key.SessionID
			snapshot.CurrentRunView = &sessionruntime.CurrentRunView{
				RunID: "running-run", TurnID: "running-turn", Generation: "gen-1", OwnerID: "owner-1", Status: status,
			}
			return snapshot, true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setRun(sessionruntime.RunStatusRunning)
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{})
	t.Cleanup(func() { _ = manager.Close() })
	fixture.service.sessionManager = manager
	fixture.service.SetAllowedTeam(driveTeamID)

	ctx := context.Background()
	if err := fixture.service.EnqueueDeferredTurn(ctx, turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  driveTeamID,
		Mode:                    turn.ModeChat,
		BotID:                   lifecycleTestBotID,
		ChatID:                  lifecycleTestBotID,
		ThreadID:                lifecycleTestSessionID,
		Query:                   directLifecyclePrompt,
		UserVisibleText:         directLifecyclePrompt,
		IdempotencyKey:          "im-message-2",
		ExternalMessageID:       "im-message-2",
		SourceChannelIdentityID: "im-sender-1",
		UserID:                  "im-user-1",
		DisplayName:             "IM Sender",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-chat-1",
		ConversationType:        "private",
	}); err != nil {
		t.Fatalf("EnqueueDeferredTurn() error = %v", err)
	}
	setRun(sessionruntime.RunStatusCompleted)
	fixture.service.startFollowUp(ctx, sessionruntime.TerminalRun{RunID: "running-run", BotID: key.BotID, SessionID: key.SessionID})
	awaitDriveLifecycle(t, fixture)
	return requireDriveRequests(t, recorder)
}

// driveResume continues a run the previous process was stopped in the middle
// of, from the intent it saved.
func driveResume(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	fixture.service.SetAllowedTeam(driveTeamID)
	raw, err := json.Marshal(map[string]any{"resume": resumeContext{
		Version:           1,
		Query:             directLifecyclePrompt,
		ChatID:            lifecycleTestBotID,
		UserID:            "im-user-1",
		ChannelIdentityID: "im-sender-1",
		CurrentChannel:    "telegram",
		ReplyTarget:       "im-chat-1",
		ConversationType:  "private",
	}})
	if err != nil {
		t.Fatal(err)
	}
	done, err := fixture.service.resumeInterruptedSession(context.Background(), sqlc.SessionRun{
		TeamID:    db.ParseUUIDOrEmpty(driveTeamID),
		RunID:     db.ParseUUIDOrEmpty(driveInterruptedRunID),
		BotID:     db.ParseUUIDOrEmpty(lifecycleTestBotID),
		SessionID: db.ParseUUIDOrEmpty(lifecycleTestSessionID),
		InputJson: raw,
	})
	if err != nil {
		t.Fatalf("resumeInterruptedSession() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resumed run did not finish")
	}
	return requireDriveRequests(t, recorder)
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
		SessionID:       lifecycleTestSessionID,
		OwnerUserID:     ownerUserID,
		ModelID:         "direct-lifecycle-model",
		RuntimeModelID:  "external-runtime-model",
		ReasoningEffort: "medium",
	}
}

// driveSchedule fires a scheduled task on a session the built-in agent runs.
func driveSchedule(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	result, err := fixture.service.TriggerSchedule(context.Background(), lifecycleTestBotID, driveSchedulePayload("schedule-owner"), "")
	if err != nil {
		t.Fatalf("TriggerSchedule() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerSchedule() status = %q, want ok", result.Status)
	}
	return requireDriveRequests(t, recorder)
}

// runDecisionContinuation runs a decision continuation with the caller's
// event channel as its end, so the resumed run can still deliver a question.
func runDecisionContinuation(t *testing.T, run func(chan<- WSStreamEvent) error) {
	t.Helper()
	eventCh := make(chan WSStreamEvent, 64)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range eventCh {
		}
	}()
	err := run(eventCh)
	close(eventCh)
	<-drained
	if err != nil {
		t.Fatalf("continuation error = %v", err)
	}
}

// driveApprovalContinuation continues a run after its tool call was approved
// and executed. The approved call runs before the answer arrives, so the
// continuation's job is only to store the result it produced as the call's
// answer and resume the model from it. The run supplies that result rather
// than executing the call again: the fixture's model is offered no
// container_exec tool, and replaying the call is what the approval path exists
// to avoid.
func driveApprovalContinuation(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	fixture.service.messageService = driveDecisionHistory(t, "container_exec", sdk.TextOutput("command finished"))
	approval := toolapproval.Request{
		ID: "tool-approval", BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID,
		ToolCallID: driveToolCallID, ToolName: "container_exec",
		SourcePlatform: "telegram", ReplyTarget: "im-chat-1", ConversationType: "private",
	}
	input := ToolApprovalResponseInput{BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID}
	result := sdk.ToolResultPart{
		ToolCallID: approval.ToolCallID, ToolName: approval.ToolName,
		Result: sdk.TextOutput("command finished"),
	}
	cont := fixture.service.toolApprovalContinuation(context.Background(), CommittedToolApprovalResponse{
		request: approval, input: input, runID: lifecycleTestRunID,
	}, true)
	cont.Setup = func(ctx context.Context, runReq ChatRequest) (runPlan, error) {
		return fixture.service.storeToolResultAndContinue(ctx, approval, input, result, nil, runReq, true)
	}
	runDecisionContinuation(t, func(eventCh chan<- WSStreamEvent) error {
		return fixture.service.driveUnroutedContinuation(context.Background(), cont, eventCh)
	})
	return requireDriveRequests(t, recorder)
}

// driveAskUserContinuation continues a run after the user answered its
// question. The answer comes back on the committed request the user input
// service resolved, so the continuation stores it as the ask_user call's
// result and runs the model on from there; storing it as a new user message
// instead would schedule a memory extraction the answer never asked for.
func driveAskUserContinuation(t *testing.T) []sdk.Request {
	t.Helper()
	fixture, recorder := newDriveRequestFixture(t)
	fixture.service.messageService = driveDecisionHistory(t, "ask_user", sdk.TextOutput(`{"answers":[{"text":"blue"}]}`))
	answered := userinput.Request{
		ID: "user-input", BotID: lifecycleTestBotID, SessionID: lifecycleTestSessionID,
		ToolCallID: driveToolCallID, ToolName: "ask_user",
		SourcePlatform: "telegram", ReplyTarget: "im-chat-1", ConversationType: "private",
		Result: map[string]any{"answers": []any{map[string]any{"text": "blue"}}},
	}
	fixture.service.userInput = &fakeUserInputService{target: answered, resolved: answered}
	committed, err := fixture.service.CommitUserInputResponse(context.Background(), UserInputResponseInput{
		BotID: lifecycleTestBotID, ThreadID: lifecycleTestSessionID, UserInputID: answered.ID,
	})
	if err != nil {
		t.Fatalf("CommitUserInputResponse() error = %v", err)
	}
	runDecisionContinuation(t, func(eventCh chan<- WSStreamEvent) error {
		return fixture.service.ContinueCommittedUserInputResponse(context.Background(), committed, eventCh)
	})
	return requireDriveRequests(t, recorder)
}

type driveRequestEntry struct {
	name  string
	drive func(t *testing.T) []sdk.Request
}

var driveRequestEntries = []driveRequestEntry{
	{name: "web_send", drive: driveWebSend},
	{name: "web_retry", drive: driveWebRetry},
	{name: "web_edit", drive: driveWebEdit},
	{name: "im_start_turn", drive: driveIMStartTurn},
	{name: "follow_up", drive: driveFollowUp},
	{name: "resume", drive: driveResume},
	{name: "schedule", drive: driveSchedule},
	{name: "approval_continuation", drive: driveApprovalContinuation},
	{name: "ask_user_continuation", drive: driveAskUserContinuation},
}

// TestDriveRequestGolden pins the model requests each entry produces.
func TestDriveRequestGolden(t *testing.T) {
	for _, entry := range driveRequestEntries {
		t.Run(entry.name, func(t *testing.T) {
			assertDriveGolden(t, "drive-requests", entry.name, marshalDriveRequests(t, entry.drive(t)))
		})
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
func newDrivePromptFixture(t *testing.T) (*Service, *drivePromptRecorder) {
	t.Helper()
	service := newACPLifecycleService(t, &recordingACPPrompter{}, &recordingMessageService{}, &recordingContextLifecycleStore{})
	service.sessionRuntime = &lifecycleTurnAdmitter{admission: lifecycleTestAdmission()}
	service.userInput = &fakeUserInputService{}
	recorder := &drivePromptRecorder{}
	service.SetExternalRuntimes(recorder)
	return service, recorder
}

// marshalDrivePrompts renders the prompts one run gave its runtime, without
// the event sink and the steering port: both are callbacks into this process,
// not something the runtime is told.
func marshalDrivePrompts(t *testing.T, inputs []external.PromptInput) []byte {
	t.Helper()
	rendered := make([]map[string]any, 0, len(inputs))
	for _, input := range inputs {
		input.Sink = nil
		input.Steering = nil
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatalf("marshal prompt input: %v", err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("decode prompt input: %v", err)
		}
		delete(fields, "Sink")
		delete(fields, "Steering")
		rendered = append(rendered, fields)
	}
	got, err := json.MarshalIndent(rendered, "", "  ")
	if err != nil {
		t.Fatalf("marshal prompt inputs: %v", err)
	}
	got = driveRequestHeaderTime.ReplaceAll(got, []byte(`t=\"<time>\"`))
	return append(got, '\n')
}

func drivePromptWebSend(t *testing.T) []external.PromptInput {
	t.Helper()
	service, recorder := newDrivePromptFixture(t)
	eventCh := make(chan WSStreamEvent)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range eventCh {
		}
	}()
	_, err := service.StreamChatWS(context.Background(), ChatRequest{
		BotID:           lifecycleTestBotID,
		ChatID:          lifecycleTestBotID,
		ThreadID:        lifecycleTestSessionID,
		RunID:           lifecycleTestRunID,
		Query:           directLifecyclePrompt,
		RawQuery:        directLifecyclePrompt,
		UserVisibleText: directLifecyclePrompt,
		CurrentChannel:  "local",
	}, eventCh, make(chan struct{}))
	close(eventCh)
	<-drained
	if err != nil {
		t.Fatalf("StreamChatWS() error = %v", err)
	}
	return recorder.recorded(t)
}

func drivePromptIMStartTurn(t *testing.T) []external.PromptInput {
	t.Helper()
	service, recorder := newDrivePromptFixture(t)
	handle, err := service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  driveTeamID,
		Mode:                    turn.ModeChat,
		BotID:                   lifecycleTestBotID,
		ChatID:                  lifecycleTestBotID,
		ThreadID:                lifecycleTestSessionID,
		Query:                   directLifecyclePrompt,
		UserVisibleText:         directLifecyclePrompt,
		ExternalMessageID:       "im-message-1",
		SourceChannelIdentityID: "im-sender-1",
		UserID:                  "user-1",
		DisplayName:             "IM Sender",
		CurrentChannel:          "telegram",
		Channels:                []string{"telegram"},
		ReplyTarget:             "im-chat-1",
		ConversationType:        "private",
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	drainDriveHandle(t, handle)
	return recorder.recorded(t)
}

func drivePromptSchedule(t *testing.T) []external.PromptInput {
	t.Helper()
	service, recorder := newDrivePromptFixture(t)
	result, err := service.TriggerSchedule(context.Background(), lifecycleTestBotID, driveSchedulePayload("user-1"), "")
	if err != nil {
		t.Fatalf("TriggerSchedule() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerSchedule() status = %q, want ok", result.Status)
	}
	return recorder.recorded(t)
}

type drivePromptEntry struct {
	name  string
	drive func(t *testing.T) []external.PromptInput
}

var drivePromptEntries = []drivePromptEntry{
	{name: "web_send", drive: drivePromptWebSend},
	{name: "im_start_turn", drive: drivePromptIMStartTurn},
	{name: "schedule", drive: drivePromptSchedule},
}

// TestDriveRequestGoldenExternalPrompts pins what an External Agent runtime is
// told for each entry that can run on one.
func TestDriveRequestGoldenExternalPrompts(t *testing.T) {
	for _, entry := range drivePromptEntries {
		t.Run(entry.name, func(t *testing.T) {
			assertDriveGolden(t, "drive-prompt-inputs", entry.name, marshalDrivePrompts(t, entry.drive(t)))
		})
	}
}
