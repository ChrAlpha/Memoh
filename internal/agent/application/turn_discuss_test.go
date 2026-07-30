package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	"github.com/memohai/memoh/internal/agent/turn"
	sessionpkg "github.com/memohai/memoh/internal/chat/thread"
	"github.com/memohai/memoh/internal/chat/timeline"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/hooks"
)

type fakeAgentStreamer struct {
	lastConfig *native.RunConfig
}

func (f *fakeAgentStreamer) Stream(_ context.Context, cfg native.RunConfig) <-chan native.StreamEvent {
	f.lastConfig = &cfg
	ch := make(chan native.StreamEvent, 1)
	ch <- native.StreamEvent{
		Type:     native.EventAgentEnd,
		Messages: json.RawMessage(`[{"role":"assistant","content":"done"}]`),
	}
	close(ch)
	return ch
}

type cancelDiscussAgentStreamer struct {
	once    sync.Once
	started chan struct{}
}

func (f *cancelDiscussAgentStreamer) Stream(ctx context.Context, _ native.RunConfig) <-chan native.StreamEvent {
	ch := make(chan native.StreamEvent, 1)
	f.once.Do(func() {
		close(f.started)
	})
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- native.StreamEvent{Type: native.EventAgentAbort}
	}()
	return ch
}

type fakeDiscussService struct {
	resolveResult      ResolveRunConfigResult
	inlineFn           func(ctx context.Context, botID string, refs []timeline.ImageAttachmentRef) []sdk.ImagePart
	storeCalls         int
	lastStoreRunID     string
	lastStoreLifecycle *contextfrag.LifecycleHolder
}

func (f *fakeDiscussService) ResolveRunConfig(_ context.Context, _, _, _, _, _, _, _ string) (ResolveRunConfigResult, error) {
	return f.resolveResult, nil
}

func (f *fakeDiscussService) InlineImageAttachments(ctx context.Context, botID string, refs []timeline.ImageAttachmentRef) []sdk.ImagePart {
	if f.inlineFn != nil {
		return f.inlineFn(ctx, botID, refs)
	}
	return nil
}

func (f *fakeDiscussService) StoreRound(_ context.Context, runID, _, _, _, _ string, _ []sdk.Message, _ string, lifecycle *contextfrag.LifecycleHolder) error {
	f.storeCalls++
	f.lastStoreRunID = runID
	f.lastStoreLifecycle = lifecycle
	return nil
}

type testAgentStreamer interface {
	Stream(context.Context, native.RunConfig) <-chan native.StreamEvent
}

type testDiscussService interface {
	ResolveRunConfig(context.Context, string, string, string, string, string, string, string) (ResolveRunConfigResult, error)
	InlineImageAttachments(context.Context, string, []timeline.ImageAttachmentRef) []sdk.ImagePart
	StoreRound(context.Context, string, string, string, string, string, []sdk.Message, string, *contextfrag.LifecycleHolder) error
}

func newDiscussTestService(streamer testChatStreamer, agent testAgentStreamer, resolver testDiscussService) *Service {
	service := newTurnTestService(streamer)
	service.turnHooks.streamAgent = agent.Stream
	service.turnHooks.resolveRunConfig = resolver.ResolveRunConfig
	service.turnHooks.inlineImages = resolver.InlineImageAttachments
	service.turnHooks.storeRound = resolver.StoreRound
	return service
}

func drainDiscuss(t *testing.T, h turn.RunHandle) []turn.Event {
	t.Helper()
	var events []turn.Event
	for e := range h.Events() {
		events = append(events, e)
	}
	for range h.Errs() {
	}
	return events
}

func assertSDKMessagesEqual(t *testing.T, got, want []sdk.Message) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got messages: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal wanted messages: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("messages = %s, want %s", gotJSON, wantJSON)
	}
}

func discussCommand() turn.StartTurnCommand {
	return turn.StartTurnCommand{
		SchemaVersion: 1,
		TeamID:        "team-1",
		Mode:          turn.ModeDiscuss,
		BotID:         "bot-1",
		ThreadID:      "sess-1",
		DiscussMessages: []turn.DiscussMessage{
			{Role: "user", Content: `<message id="1">photo</message>`},
		},
		DiscussAddressed: true,
	}
}

func TestDiscussInlinesImages(t *testing.T) {
	agent := &fakeAgentStreamer{}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: native.RunConfig{SupportsImageInput: true},
			ModelID:   "model-1",
		},
		inlineFn: func(_ context.Context, _ string, refs []timeline.ImageAttachmentRef) []sdk.ImagePart {
			if len(refs) != 1 || refs[0].ContentHash != "img-hash" {
				t.Fatalf("unexpected refs: %v", refs)
			}
			return []sdk.ImagePart{{Image: "data:image/jpeg;base64,FAKE", MediaType: "image/jpeg"}}
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)
	cmd := discussCommand()
	cmd.DiscussImageRefs = []turn.DiscussImageRef{{ContentHash: "img-hash", Mime: "image/jpeg"}}

	h, err := a.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	var userMsgs []sdk.Message
	for _, m := range agent.lastConfig.Messages {
		if m.Role == sdk.MessageRoleUser {
			userMsgs = append(userMsgs, m)
		}
	}
	if len(userMsgs) != 1 {
		t.Fatalf("expected only the canonical RC user message, got %d", len(userMsgs))
	}
	hasImage := false
	for _, part := range userMsgs[0].Content {
		if imgPart, ok := part.(sdk.ImagePart); ok {
			hasImage = true
			if !strings.HasPrefix(imgPart.Image, "data:image/jpeg;base64,") {
				t.Fatalf("unexpected image data: %q", imgPart.Image)
			}
		}
	}
	if !hasImage {
		t.Fatal("expected image part in RC user message")
	}
	if resolver.storeCalls != 1 {
		t.Fatalf("store calls = %d, want 1 after terminal agent_end", resolver.storeCalls)
	}
}

func TestDiscussUsesAdmittedRunIDInNativeConfig(t *testing.T) {
	agent := &fakeAgentStreamer{}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: native.RunConfig{},
			ModelID:   "model-1",
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)

	h, err := a.StartTurn(context.Background(), discussCommand())
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	if got := agent.lastConfig.RunID; got != h.RunID() {
		t.Fatalf("native RunID = %q, want admitted run ID %q", got, h.RunID())
	}
}

func TestAdmittedDiscussCancellationPersistsAbortedLifecycle(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000918"
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.BuildManifest(nil))
	agent := &cancelDiscussAgentStreamer{started: make(chan struct{})}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: native.RunConfig{ContextLifecycle: holder},
			ModelID:   "model-1",
		},
	}
	service := newDiscussTestService(&fakeRunner{}, agent, resolver)
	runtime := &lifecycleSubagentRuntime{runID: admittedRunID}
	lifecycles := &recordingContextLifecycleQueries{}
	service.sessionRuntime = runtime
	service.contextLifecycles = lifecycles
	cmd := discussCommand()
	cmd.BotID = lifecycleTestBotID
	cmd.ThreadID = lifecycleTestSessionID

	handle, err := service.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("admitted discuss did not reach the streaming agent")
	}
	handle.Cancel()
	drainDiscuss(t, handle)

	assertTriggerLifecycleRow(
		t,
		lifecycles,
		admittedRunID,
		contextLifecycleStatusAborted,
		"",
	)
	if len(runtime.finishes) != 1 {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", runtime.finishes)
	}
	if got := runtime.finishes[0].status; got != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finish status = %q, want %q", got, sessionruntime.RunStatusAborted)
	}
}

func TestDiscussNoInlineWhenNoVision(t *testing.T) {
	agent := &fakeAgentStreamer{}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: native.RunConfig{SupportsImageInput: false},
			ModelID:   "model-1",
		},
		inlineFn: func(_ context.Context, _ string, _ []timeline.ImageAttachmentRef) []sdk.ImagePart {
			t.Fatal("should not be called when model doesn't support vision")
			return nil
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)
	cmd := discussCommand()
	cmd.DiscussImageRefs = []turn.DiscussImageRef{{ContentHash: "img-hash", Mime: "image/jpeg"}}

	h, err := a.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	for _, m := range agent.lastConfig.Messages {
		for _, part := range m.Content {
			if _, ok := part.(sdk.ImagePart); ok {
				t.Fatal("should not have image parts when vision is not supported")
			}
		}
	}
}

func TestDiscussACPUsesChatStreamer(t *testing.T) {
	agent := &fakeAgentStreamer{}
	runner := &fakeRunner{chunks: []string{`{"type":"agent_end"}`}}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{RuntimeType: sessionpkg.RuntimeACPAgent},
	}
	a := newDiscussTestService(runner, agent, resolver)
	cmd := discussCommand()
	cmd.RouteID = "route-1"
	cmd.SourceChannelIdentityID = "acct-1"
	cmd.CurrentChannel = "telegram"
	cmd.ReplyTarget = "chat-1"
	cmd.ConversationType = "group"
	cmd.SessionToken = "Bearer owner-token"
	cmd.ChatToken = "chat-token"
	cmd.ToolHTTPURL = "http://example.test/bots/bot-1/tools"
	cmd.DiscussMessages = []turn.DiscussMessage{{Role: "user", Content: "please inspect the app"}}

	h, err := a.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	events := drainDiscuss(t, h)

	if agent.lastConfig != nil {
		t.Fatal("ordinary agent should not be invoked for ACP discuss runtime")
	}
	req := runner.gotReq
	if req.BotID != "bot-1" || req.ThreadID != "sess-1" || req.SourceChannelIdentityID != "acct-1" {
		t.Fatalf("runtime request = %#v", req)
	}
	if req.RouteID != "route-1" || req.ChatToken != "chat-token" || req.Token != "Bearer owner-token" {
		t.Fatalf("runtime context = route %q chat token %q token %q", req.RouteID, req.ChatToken, req.Token)
	}
	if req.ToolHTTPURL != "http://example.test/bots/bot-1/tools" {
		t.Fatalf("ToolHTTPURL = %q", req.ToolHTTPURL)
	}
	if !strings.Contains(req.Query, "please inspect the app") || !strings.Contains(req.Query, "reset each turn") || !strings.Contains(req.Query, "MUST use the `send` tool") {
		t.Fatalf("runtime query = %q, want full discuss context", req.Query)
	}
	if strings.Contains(req.Query, "Current time:") || strings.Contains(req.Query, "addressed directly") {
		t.Fatalf("runtime query contains volatile late-binding context: %q", req.Query)
	}
	if strings.Index(req.Query, "MUST use the `send` tool") > strings.Index(req.Query, "please inspect the app") {
		t.Fatalf("ACP send contract must stay in the stable preamble: %q", req.Query)
	}
	if !req.UserMessagePersisted {
		t.Fatal("runtime request should avoid duplicating the full-context prompt as a user history message")
	}
	if !req.ForceFreshRuntime {
		t.Fatal("discuss ACP runtime request should force a fresh runtime each turn")
	}
	var sawTerminal bool
	for _, e := range events {
		if e.Kind == "agent_end" {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Fatal("expected terminal agent_end event forwarded from the runtime")
	}
}

func TestDiscussACPSkipsWhenNotAddressed(t *testing.T) {
	const admittedRunID = "88888888-8888-4888-8888-888888888888"
	agent := &fakeAgentStreamer{}
	runner := &fakeRunner{chunks: []string{`{"type":"agent_end"}`}}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{RuntimeType: sessionpkg.RuntimeACPAgent},
	}
	a := newDiscussTestService(runner, agent, resolver)
	runtime := &lifecycleSubagentRuntime{runID: admittedRunID}
	lifecycles := &recordingContextLifecycleQueries{}
	a.sessionRuntime = runtime
	a.contextLifecycles = lifecycles
	cmd := discussCommand()
	cmd.BotID = lifecycleTestBotID
	cmd.ThreadID = lifecycleTestSessionID
	cmd.DiscussAddressed = false

	h, err := a.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	events := drainDiscuss(t, h)

	if runner.gotReq.BotID != "" {
		t.Fatal("runtime must not start for a passive (unaddressed) message")
	}
	var sawSkip bool
	for _, e := range events {
		if e.Kind == turn.DiscussEventSkipped {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Fatal("expected skip marker event")
	}
	if len(lifecycles.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(lifecycles.params))
	}
	if got := pgUUIDString(lifecycles.params[0].RunID); got != admittedRunID {
		t.Fatalf("context lifecycle run ID = %q, want admitted ID %q", got, admittedRunID)
	}
	if got := lifecycles.params[0].Status; got != contextLifecycleStatusCompleted {
		t.Fatalf("context lifecycle status = %q, want %q", got, contextLifecycleStatusCompleted)
	}
}

func TestDiscussRefreshesContextFragWithoutLateBindingMessage(t *testing.T) {
	agent := &fakeAgentStreamer{}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: native.RunConfig{System: "base system"},
			ModelID:   "model-1",
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)
	cmd := discussCommand()
	cmd.DiscussMessages = []turn.DiscussMessage{{Role: "user", Content: `<message id="x">hello</message>`}}

	h, err := a.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be invoked")
	}
	cfg := agent.lastConfig
	if cfg.ContextManifest.Counts.Messages != len(cfg.Messages) {
		t.Fatalf("manifest message count = %d, messages = %d", cfg.ContextManifest.Counts.Messages, len(cfg.Messages))
	}
	if len(cfg.Messages) != 1 {
		t.Fatalf("messages = %d, want only composed discuss context", len(cfg.Messages))
	}
	if lastMessageFragContains(cfg.ContextFrags, "Current time:") ||
		lastMessageFragContains(cfg.ContextFrags, "MUST use the `send` tool") {
		t.Fatalf("context frags include a volatile late-binding prompt: %#v", cfg.ContextManifest.Items)
	}
}

// TestDiscussPropagatesContextBudgetAndToolExchangePolicy proves the
// sibling ContextBudgetMaxTokens result field (ResolveRunConfig cannot set
// it on the inner RunConfig itself: buildBaseRunConfig runs before the
// discuss turn has any Messages to size a budget against) reaches the
// streamed RunConfig, and that discuss turns get a default tool-exchange
// policy like chat turns do.
func TestDiscussPropagatesContextBudgetAndToolExchangePolicy(t *testing.T) {
	agent := &fakeAgentStreamer{}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig:              native.RunConfig{},
			ModelID:                "model-1",
			ContextBudgetMaxTokens: 128000,
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)

	h, err := a.StartTurn(context.Background(), discussCommand())
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	if agent.lastConfig.ContextBudgetMaxTokens != 128000 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want 128000 (resolved.ContextBudgetMaxTokens must propagate into the streamed RunConfig)", agent.lastConfig.ContextBudgetMaxTokens)
	}
	if agent.lastConfig.ContextToolExchangePolicy == nil {
		t.Fatal("expected a default ContextToolExchangePolicy for the discuss path")
	}
}

func TestDiscussCarriesComposedMessagesThroughTypedFragments(t *testing.T) {
	agent := &fakeAgentStreamer{}
	baseConfig := native.RunConfig{
		System:          "base system",
		ContextHookText: "hook context",
		ContextScope:    contextfrag.Scope{BotID: "bot-1", SessionID: "sess-1"},
	}
	baseConfig.ContextSourceFrags = contextview.CollectProviderSourceFrags(context.Background(), baseConfig)
	baseConfig.ContextSourceFrags = append(baseConfig.ContextSourceFrags, contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "stale.message",
		Message:    sdk.UserMessage("must not survive"),
		Kind:       contextfrag.KindConversationEvent,
		Slot:       contextfrag.SlotHistory,
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustExternal,
	}))
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: baseConfig,
			ModelID:   "model-1",
		},
		inlineFn: func(_ context.Context, _ string, _ []timeline.ImageAttachmentRef) []sdk.ImagePart {
			return []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}}
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)
	cmd := discussCommand()
	cmd.DiscussMessages = []turn.DiscussMessage{
		{Role: "user", Content: "first user"},
		{Role: "user", Content: "second user"},
		{
			Role:                 "user",
			Content:              "<summary>\ncovered history\n</summary>",
			CompactionArtifactID: "artifact-1",
		},
		{
			Role:       "tool",
			Content:    "debug fallback",
			RawContent: json.RawMessage(`[{"type":"tool-result","toolCallId":"call-1","toolName":"lookup","result":{"answer":42}}]`),
		},
		{Role: "user", Content: "latest user"},
	}
	cmd.DiscussImageRefs = []turn.DiscussImageRef{{ContentHash: "image-1", Mime: "image/png"}}

	h, err := a.StartTurn(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	frags := agent.lastConfig.ContextSourceFrags
	if len(frags) != len(cmd.DiscussMessages)+2 {
		t.Fatalf("ContextSourceFrags = %d, want system + %d discuss + hook", len(frags), len(cmd.DiscussMessages))
	}
	if frags[0].Slot != contextfrag.SlotSystem {
		t.Fatalf("first frag slot = %q, want system", frags[0].Slot)
	}
	wantIDs := []string{
		"discuss.message.000",
		"discuss.message.001",
		"discuss.message.002",
		"discuss.message.003",
		"discuss.message.004",
	}
	for i, wantID := range wantIDs {
		if frags[i+1].ID != wantID {
			t.Fatalf("discuss frag %d ID = %q, want %q", i, frags[i+1].ID, wantID)
		}
	}
	if last := frags[len(frags)-1]; last.Kind != contextfrag.KindHookContext || last.Slot != contextfrag.SlotAfterHistoryBeforeCurrent {
		t.Fatalf("last frag = %q/%q, want hook after history", last.Kind, last.Slot)
	}
	for _, frag := range frags {
		if frag.ID == "stale.message" {
			t.Fatal("pre-compose history fragment survived the authoritative discuss carrier")
		}
	}

	rendered, err := contextview.ApplyProviderRunConfig(
		context.Background(),
		slog.New(slog.DiscardHandler),
		*agent.lastConfig,
	)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v", err)
	}
	if rendered.System != baseConfig.System {
		t.Fatalf("System = %q, want %q", rendered.System, baseConfig.System)
	}
	wantMessages := append([]sdk.Message(nil), agent.lastConfig.Messages...)
	wantMessages = append(wantMessages, sdk.UserMessage(baseConfig.ContextHookText))
	assertSDKMessagesEqual(t, rendered.Messages, wantMessages)
}

func TestDiscussRetainsResolvedHookSystemSections(t *testing.T) {
	agent := &fakeAgentStreamer{}
	baseConfig := native.RunConfig{
		System:       "base system",
		ContextScope: contextfrag.Scope{BotID: "bot-1", SessionID: "sess-1"},
	}
	baseConfig.ContextSourceFrags = contextview.CollectProviderSourceFrags(context.Background(), baseConfig)
	hookBuild := buildHookSystemSections([]promptHookOutput{{
		Event: hooks.EventBeforePromptBuild,
		Result: hooks.Result{AppendSystemSections: []hooks.SystemSectionOutput{{
			HookName: "round-seven",
			Text:     "hook system",
		}}},
	}}, baseConfig.ContextScope)
	baseConfig.ContextSourceFrags = append(baseConfig.ContextSourceFrags, hookBuild.Frags...)
	resolver := &fakeDiscussService{resolveResult: ResolveRunConfigResult{
		RunConfig: baseConfig,
		ModelID:   "model-1",
	}}
	service := newDiscussTestService(&fakeRunner{}, agent, resolver)

	handle, err := service.StartTurn(context.Background(), discussCommand())
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, handle)

	if agent.lastConfig == nil {
		t.Fatal("expected agent to be called")
	}
	rendered, err := contextview.ApplyProviderRunConfig(
		context.Background(),
		slog.New(slog.DiscardHandler),
		*agent.lastConfig,
	)
	if err != nil {
		t.Fatalf("ApplyProviderRunConfig() error = %v", err)
	}
	if rendered.System != "base system\n\nhook system" {
		t.Fatalf("System = %q, want resolved hook system section", rendered.System)
	}
}

// TestDiscussStoresRoundWithContextLifecycle proves the resolved
// ContextLifecycle (defaulted when ResolveRunConfig's fake returns none)
// reaches persistence, matching StoreRoundWithContextLifecycle's chat-mode
// contract instead of the lifecycle-blind StoreRound.
func TestDiscussStoresRoundWithContextLifecycle(t *testing.T) {
	agent := &fakeAgentStreamer{}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{
			RunConfig: native.RunConfig{},
			ModelID:   "model-1",
		},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)

	h, err := a.StartTurn(context.Background(), discussCommand())
	if err != nil {
		t.Fatal(err)
	}
	drainDiscuss(t, h)

	if resolver.storeCalls != 1 {
		t.Fatalf("store calls = %d, want 1", resolver.storeCalls)
	}
	if resolver.lastStoreLifecycle == nil {
		t.Fatal("expected the defaulted ContextLifecycle to reach StoreRoundWithContextLifecycle")
	}
	if resolver.lastStoreRunID != h.RunID() {
		t.Fatalf("persisted discuss RunID = %q, want admitted run ID %q", resolver.lastStoreRunID, h.RunID())
	}
}

func TestStoreDiscussRoundPersistsAdmittedRunID(t *testing.T) {
	messages := &recordingMessageService{}
	service := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}

	err := service.storeDiscussRound(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		"",
		"local",
		[]sdk.Message{sdk.AssistantMessage("done")},
		"model-id",
		nil,
	)
	if err != nil {
		t.Fatalf("storeDiscussRound() error = %v", err)
	}
	if len(messages.persisted) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(messages.persisted))
	}
	if got := messages.persisted[0].RunID; got != lifecycleTestRunID {
		t.Fatalf("persisted discuss RunID = %q, want admitted ID %q", got, lifecycleTestRunID)
	}
}

// TestDiscussACPFullContextPromptGoldenFormat pins the legacy discuss-ACP
// prompt format discussACPFullContextPrompt still produces: the discuss-ACP
// path has no RC/TR streams available at this layer (turn.StartTurnCommand
// only carries flattened turn.DiscussMessage) to build a contextview-backed
// prompt from, so this remains the only prompt-assembly path for ACP
// discuss runtimes.
func TestDiscussACPFullContextPromptGoldenFormat(t *testing.T) {
	got := discussACPFullContextPrompt([]turn.DiscussMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "", Content: "anon"},
		{Role: "tool", Content: ""},
	})

	const want = "You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n" +
		"IMPORTANT: You MUST use the `send` tool to speak in the observed conversation. Ordinary text output is internal and invisible to everyone.\n\n" +
		"[user]\nhello\n\n" +
		"[assistant]\nhi\n\n" +
		"[user]\nanon\n\n" +
		"Reply to the latest user-visible message when a response is appropriate."
	if got != want {
		t.Fatalf("legacy discuss ACP prompt format drifted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestInjectImagePartsIntoLastUserMessage(t *testing.T) {
	msgs := []sdk.Message{
		sdk.UserMessage("hello"),
		sdk.AssistantMessage("hi"),
		sdk.UserMessage("look at this"),
	}
	parts := []sdk.ImagePart{
		{Image: "data:image/png;base64,abc", MediaType: "image/png"},
	}

	injectImagePartsIntoLastUserMessage(msgs, parts)

	lastUser := msgs[2]
	if len(lastUser.Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(lastUser.Content))
	}
	imgPart, ok := lastUser.Content[1].(sdk.ImagePart)
	if !ok {
		t.Fatalf("expected ImagePart, got %T", lastUser.Content[1])
	}
	if imgPart.Image != "data:image/png;base64,abc" {
		t.Fatalf("unexpected image: %q", imgPart.Image)
	}
}

func TestInjectImagePartsIntoLastUserMessage_Empty(t *testing.T) {
	msgs := []sdk.Message{sdk.UserMessage("hello")}
	injectImagePartsIntoLastUserMessage(msgs, nil)
	if len(msgs[0].Content) != 1 {
		t.Fatalf("expected no change, got %d parts", len(msgs[0].Content))
	}
}

func TestInjectImagePartsIntoLastUserMessage_SkipsEmptyImage(t *testing.T) {
	msgs := []sdk.Message{sdk.UserMessage("hello")}
	parts := []sdk.ImagePart{{Image: "", MediaType: "image/png"}}
	injectImagePartsIntoLastUserMessage(msgs, parts)
	if len(msgs[0].Content) != 1 {
		t.Fatalf("expected no change, got %d parts", len(msgs[0].Content))
	}
}

func lastMessageFragContains(frags []contextfrag.ContextFrag, needle string) bool {
	for i := len(frags) - 1; i >= 0; i-- {
		frag := frags[i]
		if frag.Kind != contextfrag.KindConversationEvent || len(frag.Parts) == 0 || frag.Parts[0].SDKMessage == nil {
			continue
		}
		for _, part := range frag.Parts[0].SDKMessage.Content {
			if text, ok := part.(sdk.TextPart); ok && strings.Contains(text.Text, needle) {
				return true
			}
		}
		return false
	}
	return false
}

// TestDiscussCancelUnblocksFullEventBuffer mirrors the chat-mode burst
// repro for the discuss pump's emit path.
func TestDiscussCancelUnblocksFullEventBuffer(t *testing.T) {
	agent := &burstAgentStreamer{count: 40}
	resolver := &fakeDiscussService{
		resolveResult: ResolveRunConfigResult{ModelID: "model-1"},
	}
	a := newDiscussTestService(&fakeRunner{}, agent, resolver)
	h, err := a.StartTurn(context.Background(), discussCommand())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	h.Cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				for range h.Errs() {
				}
				return
			}
		case <-deadline:
			t.Fatal("discuss events channel not closed after cancel with full buffer")
		}
	}
}

type burstAgentStreamer struct {
	count int
}

func (f *burstAgentStreamer) Stream(ctx context.Context, _ native.RunConfig) <-chan native.StreamEvent {
	ch := make(chan native.StreamEvent)
	go func() {
		defer close(ch)
		for range f.count {
			select {
			case ch <- native.StreamEvent{Type: native.EventTextDelta, Delta: "x"}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}
