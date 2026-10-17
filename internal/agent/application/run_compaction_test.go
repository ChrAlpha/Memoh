package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/thread"
)

// overHardShare is a request past the hard share of syncTestBudget; the
// fixed part (system prompt, tools) is what no rebuilt context can shrink.
var overHardShare = native.StepContextInput{FixedTokens: 100, Messages: conversationOf(800)}

// conversationOf is a conversation the estimator prices at about tokens.
func conversationOf(tokens int) []sdk.Message {
	text := ""
	for runreq.RequestInputTokens(0, []sdk.Message{sdk.UserMessage(text)}) < tokens {
		text += "conversation "
	}
	return []sdk.Message{sdk.UserMessage(text)}
}

func newStepCompactionFixture(t *testing.T, status string) (stepCompaction, *scriptedCompactionRunner, *stagedHistory) {
	t.Helper()
	service, runner, history, req := newSyncCompactionFixture(t, status)
	history.big = []messagepkg.Message{
		agenttest.AssembleText(t, "old-step", "assistant", strings.Repeat("x", 4000), agenttest.AssembleBase),
		agenttest.AssembleText(t, "request", "user", "the request", agenttest.AssembleBase.Add(time.Second)),
	}
	history.small = []messagepkg.Message{
		agenttest.AssembleText(t, "summary", "assistant", "<summary>short</summary>", agenttest.AssembleBase),
		agenttest.AssembleText(t, "request", "user", "the request", agenttest.AssembleBase.Add(time.Second)),
	}
	return stepCompaction{
		service:          service,
		req:              req,
		requestMessageID: func() string { return "request" },
		budget:           syncTestBudget,
		extras:           []sdk.Message{sdk.UserMessage("recalled memory")},
	}, runner, history
}

func sdkTexts(messages []sdk.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		var text strings.Builder
		for _, part := range message.Content {
			if value, ok := part.(sdk.TextPart); ok {
				text.WriteString(value.Text)
			}
		}
		texts = append(texts, text.String())
	}
	return texts
}

func assertSDKTexts(t *testing.T, got []sdk.Message, want ...string) {
	t.Helper()
	texts := sdkTexts(got)
	if len(texts) != len(want) {
		t.Fatalf("messages = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("messages = %q, want %q", texts, want)
		}
	}
}

// The rebuilt conversation is the stored rows after the summary: the
// compacted steps are gone, the user's message is still there, and what the
// run had added in front of it stays in front of it.
func TestStepCompactionRebuildsTheConversationFromTheRowsAfterCompacting(t *testing.T) {
	t.Parallel()

	run, runner, history := newStepCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted

	got := run.compact(context.Background(), overHardShare)
	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once", runner.callCount())
	}
	assertSDKTexts(t, got, "<summary>short</summary>", "recalled memory", "the request")
}

// Only a pass that produced a summary changes what the next call sends.
func TestStepCompactionKeepsTheConversationUnlessCompactionSucceeded(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status string
		err    error
	}{
		{"nothing to compact or cooldown", compaction.StatusNoop, nil},
		{"failure", "", errors.New("summarizer unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run, runner, history := newStepCompactionFixture(t, tc.status)
			runner.err = tc.err
			// The rows would shrink if the run rebuilt after a failed pass.
			runner.onRun = history.markCompacted
			loads := history.AfterCalls

			if got := run.compact(context.Background(), overHardShare); got != nil {
				t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
			}
			if runner.callCount() != 1 {
				t.Fatalf("compaction ran %d times, want once", runner.callCount())
			}
			if history.AfterCalls != loads {
				t.Fatal("the rows were read again after a pass that produced no summary")
			}
		})
	}
}

func TestStepCompactionLeavesAConversationBelowTheHardShareAlone(t *testing.T) {
	t.Parallel()

	run, runner, history := newStepCompactionFixture(t, compaction.StatusOK)
	loads := history.AfterCalls

	below := native.StepContextInput{FixedTokens: 100, Messages: conversationOf(600)}
	if got := run.compact(context.Background(), below); got != nil {
		t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
	}
	if runner.callCount() != 0 || history.AfterCalls != loads {
		t.Fatalf("calls=%d loads=%d->%d: below the hard share nothing runs", runner.callCount(), loads, history.AfterCalls)
	}
}

// A rebuilt context that is no smaller than the current one is not worth the
// lost prefix.
func TestStepCompactionKeepsTheConversationWhenTheRebuildIsNoSmaller(t *testing.T) {
	t.Parallel()

	run, runner, history := newStepCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted

	small := native.StepContextInput{FixedTokens: 760}
	if got := run.compact(context.Background(), small); got != nil {
		t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
	}
}

// A compaction already running is waited for and its summary used, without
// a second pass.
func TestStepCompactionUsesARunningCompactionsSummary(t *testing.T) {
	t.Parallel()

	run, runner, history := newStepCompactionFixture(t, compaction.StatusOK)
	running := run.service.enterSessionCompaction(run.req.BotID, run.req.ThreadID)
	done := make(chan []sdk.Message, 1)
	go func() { done <- run.compact(context.Background(), overHardShare) }()

	select {
	case <-done:
		t.Fatal("the run went on while a compaction held the session")
	case <-time.After(50 * time.Millisecond):
	}
	history.markCompacted()
	running()

	select {
	case got := <-done:
		assertSDKTexts(t, got, "<summary>short</summary>", "recalled memory", "the request")
	case <-time.After(2 * time.Second):
		t.Fatal("the run did not resume after the compaction finished")
	}
	if runner.callCount() != 0 {
		t.Fatalf("compaction ran %d times, want none: the one in flight had made room", runner.callCount())
	}
}

func TestEphemeralRunMessagesAreTheOnesBetweenHistoryAndTheUserMessage(t *testing.T) {
	t.Parallel()

	index := 4
	cfg := native.RunConfig{
		Messages: []sdk.Message{
			sdk.UserMessage("h1"), sdk.AssistantMessage("h2"),
			sdk.UserMessage("notice"), sdk.UserMessage("memory"), sdk.UserMessage("the query"),
		},
		ContextTrimmableMessages:       2,
		ContextCurrentUserMessageIndex: &index,
	}
	assertSDKTexts(t, ephemeralRunMessages(cfg), "notice", "memory")

	// A continuation holds nothing but rows.
	cfg.ContextTrimmableMessages = len(cfg.Messages)
	if got := ephemeralRunMessages(cfg); got != nil {
		t.Fatalf("messages = %q, want none", sdkTexts(got))
	}
}

func TestBindStepCompactionNeedsCommittedSteps(t *testing.T) {
	t.Parallel()

	service, _, _, req := newSyncCompactionFixture(t, compaction.StatusOK)
	committer := &persist.StepCommitter{}
	cases := []struct {
		name      string
		committer *persist.StepCommitter
		mutate    func(*runreq.ChatRequest)
		want      bool
	}{
		{"committed steps", committer, func(*runreq.ChatRequest) {}, true},
		{"steps not committed", nil, func(*runreq.ChatRequest) {}, false},
		{"cutoff without a replacement", committer, func(r *runreq.ChatRequest) { r.HistoryCutoffBeforeMessageID = "m-1" }, false},
		{"retry or edit", committer, func(r *runreq.ChatRequest) {
			r.HistoryCutoffBeforeMessageID = "m-1"
			r.TurnReplacement = &messagepkg.TurnReplacement{}
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := req
			tc.mutate(&r)
			var cfg native.RunConfig
			service.bindStepCompaction(&cfg, r, tc.committer)
			if got := cfg.StepContextCompactor != nil; got != tc.want {
				t.Fatalf("compactor bound = %v, want %v", got, tc.want)
			}
		})
	}
}

// scriptedModel answers the first call with a tool call and every later one
// with an answer, keeping the requests it was sent.
type scriptedModel struct {
	mu       sync.Mutex
	requests []sdk.Request
}

func (*scriptedModel) Name() string                                    { return "scripted" }
func (*scriptedModel) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }
func (*scriptedModel) Test(context.Context) error                      { return nil }
func (*scriptedModel) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (m *scriptedModel) DoGenerate(_ context.Context, req sdk.Request) (sdk.ModelResult, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	call := len(m.requests)
	m.mu.Unlock()
	if call == 1 {
		return sdk.ModelResult{
			FinishReason: sdk.FinishReasonToolCalls,
			ToolCalls: []sdk.ToolCall{{
				ToolCallID: "call-1", ToolName: "lookup",
				Input: toolexec.ArgumentsFromValue(map[string]any{}),
			}},
		}, nil
	}
	return sdk.ModelResult{Text: "done", FinishReason: sdk.FinishReasonStop}, nil
}

func (m *scriptedModel) DoStream(ctx context.Context, req sdk.Request) (<-chan sdk.StreamPart, error) {
	result, err := m.DoGenerate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.StreamPart, 8)
	ch <- &sdk.StartPart{}
	ch <- &sdk.StartStepPart{}
	if result.Text != "" {
		ch <- &sdk.TextStartPart{ID: "answer"}
		ch <- &sdk.TextDeltaPart{ID: "answer", Text: result.Text}
		ch <- &sdk.TextEndPart{ID: "answer"}
	}
	for _, call := range result.ToolCalls {
		ch <- &sdk.StreamToolCallPart{ToolCallID: call.ToolCallID, ToolName: call.ToolName, Input: toolexec.ArgumentsFromValue(call.Input)}
	}
	ch <- &sdk.FinishStepPart{FinishReason: result.FinishReason}
	ch <- &sdk.FinishPart{FinishReason: result.FinishReason}
	close(ch)
	return ch, nil
}

type bulkyToolProvider struct{}

func (bulkyToolProvider) Tools(context.Context, agenttools.SessionContext) ([]toolexec.Tool, error) {
	return []toolexec.Tool{{
		Name:       "lookup",
		Parameters: &jsonschema.Schema{Type: "object"},
		Execute: func(_ *toolexec.ToolExecContext, _ sdk.ToolArguments) (sdk.ToolOutput, error) {
			return toolexec.OutputFromValue(map[string]any{"answer": strings.Repeat("tool-result ", 800)}), nil
		},
	}}, nil
}

// A run that started with an image keeps it through a compaction: the model's
// next request still carries the image, on the user's message, after the
// summary that replaced the steps.
func TestStepCompactionKeepsTheRunsInlineImageOnTheNextRequest(t *testing.T) {
	t.Parallel()

	run, runner, history := newStepCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	image := sdk.ImagePart{Image: "data:image/png;base64,AAAA"}
	run.inlineParts = []sdk.MessagePart{image}
	// The image alone is priced below the hard share; with the tool result
	// of the first step the second call is over it.
	run.budget = 4000

	model := &scriptedModel{}
	agent := native.New(native.Deps{})
	agent.SetToolProviders([]agenttools.ToolProvider{bulkyToolProvider{}})
	cfg := native.RunConfig{
		Model:                &sdk.Model{ID: "scripted-model", Provider: model},
		Messages:             []sdk.Message{sdk.UserMessage("the request", image)},
		SupportsToolCall:     true,
		SupportsImageInput:   true,
		Identity:             native.SessionContext{BotID: run.req.BotID},
		StepContextCompactor: run.compact,
	}
	for range agent.Stream(context.Background(), cfg) { //nolint:revive // the events are not under test
	}

	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once", runner.callCount())
	}
	if len(model.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(model.requests))
	}
	first, second := model.requests[0].Messages, model.requests[1].Messages
	if !requestCarriesImage(first, image) {
		t.Fatalf("the first request lost its image: %#v", first)
	}
	if got := sdkTexts(second); len(got) != 3 || got[0] != "<summary>short</summary>" || got[1] != "recalled memory" || got[2] != "the request" {
		t.Fatalf("second request = %q, want the rebuilt conversation", got)
	}
	if !requestCarriesImage(second, image) {
		t.Fatalf("the request after the compaction lost the run's image: %#v", second)
	}
}

func requestCarriesImage(messages []sdk.Message, image sdk.ImagePart) bool {
	for _, message := range messages {
		if message.Role == sdk.MessageRoleUser && containsPart(message.Content, image) {
			return true
		}
	}
	return false
}

func TestAttachInlineParts(t *testing.T) {
	t.Parallel()

	image := sdk.ImagePart{Image: "data:image/png;base64,AAAA"}
	t.Run("the found message gets the media once", func(t *testing.T) {
		t.Parallel()
		messages := []sdk.Message{sdk.UserMessage("request"), sdk.AssistantMessage("answer")}
		attachInlineParts(messages, []sdk.MessagePart{image}, true)
		attachInlineParts(messages, []sdk.MessagePart{image}, true)
		if len(messages[0].Content) != 2 || !containsPart(messages[0].Content, image) {
			t.Fatalf("content = %#v, want the text and the image once", messages[0].Content)
		}
	})
	t.Run("without the row the last user message gets it", func(t *testing.T) {
		t.Parallel()
		messages := []sdk.Message{sdk.UserMessage("old"), sdk.AssistantMessage("a"), sdk.UserMessage("latest")}
		attachInlineParts(messages, []sdk.MessagePart{image}, false)
		if containsPart(messages[0].Content, image) || !containsPart(messages[2].Content, image) {
			t.Fatalf("media went to the wrong message: %#v", messages)
		}
	})
}

// A spawned run's thread is compacted like a chat's, and the parent context a
// forked subagent started from, which no row of the thread holds, stays in
// front of the rebuilt conversation.
func TestSubagentStepCompactorRebuildsThreadWithItsForkContext(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	history.big = []messagepkg.Message{
		agenttest.AssembleText(t, "old-step", "assistant", strings.Repeat("x", 4000), agenttest.AssembleBase),
		agenttest.AssembleText(t, "task", "user", "the task", agenttest.AssembleBase.Add(time.Second)),
	}
	history.small = []messagepkg.Message{
		agenttest.AssembleText(t, "summary", "assistant", "<summary>short</summary>", agenttest.AssembleBase),
		agenttest.AssembleText(t, "task", "user", "the task", agenttest.AssembleBase.Add(time.Second)),
	}
	forkMessage, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": "parent context"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.sessionService = &agenttest.SubagentSessions{
		ForkContext: []thread.SubagentForkContextMessage{{Role: "user", Message: forkMessage}},
	}
	rewireComponents(service)
	ctx := withSubagentRunHandle(context.Background(), sessionruntime.RunHandle{RunID: "sub-run", FencingToken: 1})

	compactor := service.SubagentStepCompactor(ctx, native.SpawnCompaction{
		BotID: req.BotID, SessionID: req.ThreadID, TurnRequestMessageID: "task",
		ContextBudgetMaxTokens: syncTestBudget,
	})
	if compactor == nil {
		t.Fatal("a spawned run with committed steps got no compactor")
	}
	got := compactor(ctx, overHardShare)
	assertSDKTexts(t, got, "parent context", "<summary>short</summary>", "the task")

	if service.SubagentStepCompactor(context.Background(), native.SpawnCompaction{}) != nil {
		t.Fatal("a context without an admitted run handle must get no compactor")
	}
}

// The visible history a retry starts from: two rows before the cutoff, then
// the turn being replaced.
var replacementHistory = []string{strings.Repeat("x", 4000), strings.Repeat("y", 4000)}

func newReplacementCompactionFixture(t *testing.T, status string) (*stepCompaction, *scriptedCompactionRunner, *stagedHistory) {
	t.Helper()
	service, runner, history, req := newSyncCompactionFixture(t, status)
	oldTurn := []messagepkg.Message{
		agenttest.AssembleText(t, "old-request", "user", "the replaced request", agenttest.AssembleBase.Add(time.Second)),
		agenttest.AssembleText(t, "old-answer", "assistant", "the replaced answer", agenttest.AssembleBase.Add(2*time.Second)),
	}
	history.big = append([]messagepkg.Message{
		agenttest.AssembleText(t, "old-history-1", "assistant", replacementHistory[0], agenttest.AssembleBase),
		agenttest.AssembleText(t, "old-history-2", "assistant", replacementHistory[1], agenttest.AssembleBase.Add(time.Millisecond)),
	}, oldTurn...)
	history.small = append([]messagepkg.Message{
		agenttest.AssembleText(t, "summary", "assistant", "<summary>short</summary>", agenttest.AssembleBase),
	}, oldTurn...)
	req.HistoryCutoffBeforeMessageID = "old-request"
	req.TurnReplacement = &messagepkg.TurnReplacement{}
	return &stepCompaction{
		service:          service,
		req:              req,
		requestMessageID: func() string { return "" },
		budget:           syncTestBudget,
		replacement:      true,
		tailStart:        len(replacementHistory),
	}, runner, history
}

// What a retry holds after one tool step: the visible history it started
// from, the user's message with an image, and the step.
func replacementLoopMessages(image sdk.ImagePart) []sdk.Message {
	return []sdk.Message{
		sdk.AssistantMessage(replacementHistory[0]),
		sdk.AssistantMessage(replacementHistory[1]),
		sdk.UserMessage("the request", image),
		sdk.AssistantMessage("calling lookup"),
		sdk.UserMessage("lookup result"),
	}
}

// loopInput is what the loop hands its compactor for messages.
func loopInput(messages []sdk.Message) native.StepContextInput {
	return native.StepContextInput{FixedTokens: 100, Messages: messages}
}

// A retry compacts the visible history before its cutoff and carries its own
// messages over from the loop. The rows after the cutoff belong to the turn
// being replaced and stay out, and nothing the run has executed is read from
// storage, where it would not be visible.
func TestStepCompactionOfAReplacementRunCompactsHistoryBeforeTheCutoffOnly(t *testing.T) {
	t.Parallel()

	run, runner, history := newReplacementCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	image := sdk.ImagePart{Image: "data:image/png;base64,AAAA"}

	got := run.compact(context.Background(), loopInput(replacementLoopMessages(image)))

	if runner.callCount() != 1 {
		t.Fatalf("compaction ran %d times, want once", runner.callCount())
	}
	assertSDKTexts(t, got, "<summary>short</summary>", "the request", "calling lookup", "lookup result")
	if !requestCarriesImage(got, image) {
		t.Fatalf("the user's message lost its image: %#v", got)
	}
	for _, text := range sdkTexts(got) {
		if strings.Contains(text, "replaced") {
			t.Fatalf("a row of the replaced turn reached the request: %q", sdkTexts(got))
		}
	}

	// The next boundary starts from the rebuilt conversation; the run's own
	// messages now begin right after the summary.
	next := append(append([]sdk.Message(nil), got...),
		sdk.AssistantMessage("calling lookup again"), sdk.UserMessage("second result"))
	if again := run.compact(context.Background(), loopInput(next)); again != nil {
		t.Fatalf("second compaction = %q, want the conversation kept as it is", sdkTexts(again))
	}
}

func TestStepCompactionOfAReplacementRunKeepsTheConversationOnFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status string
		err    error
	}{
		{"nothing to compact or cooldown", compaction.StatusNoop, nil},
		{"failure", "", errors.New("summarizer unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run, runner, history := newReplacementCompactionFixture(t, tc.status)
			runner.err = tc.err
			runner.onRun = history.markCompacted
			loads := history.AfterCalls

			got := run.compact(context.Background(), loopInput(replacementLoopMessages(sdk.ImagePart{Image: "data:image/png;base64,AAAA"})))
			if got != nil {
				t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
			}
			if runner.callCount() != 1 || history.AfterCalls != loads {
				t.Fatalf("calls=%d loads=%d->%d, want one pass and no rebuild", runner.callCount(), loads, history.AfterCalls)
			}
		})
	}
}

// A loop shorter than where the run's messages were recorded to start has
// none of them to carry over; rebuilding would send the history without the
// user's message.
func TestStepCompactionOfAReplacementRunNeedsItsOwnMessages(t *testing.T) {
	t.Parallel()

	run, runner, history := newReplacementCompactionFixture(t, compaction.StatusOK)
	runner.onRun = history.markCompacted
	loop := replacementLoopMessages(sdk.ImagePart{Image: "data:image/png;base64,AAAA"})
	run.tailStart = len(loop) + 1

	if got := run.compact(context.Background(), loopInput(loop)); got != nil {
		t.Fatalf("messages = %q, want the conversation kept", sdkTexts(got))
	}
}

// A retry answers its request again: the request row stays, before the
// cutoff at the old answer, and carries the run's media after a rebuild.
// The old answer is the replaced turn's and stays out.
func TestStepCompactionOfARetryKeepsItsRequestRowAndMedia(t *testing.T) {
	t.Parallel()

	service, runner, history, req := newSyncCompactionFixture(t, compaction.StatusOK)
	visible := []messagepkg.Message{
		agenttest.AssembleText(t, "old-history-1", "assistant", replacementHistory[0], agenttest.AssembleBase),
		agenttest.AssembleText(t, "old-history-2", "assistant", replacementHistory[1], agenttest.AssembleBase.Add(time.Millisecond)),
	}
	request := agenttest.AssembleText(t, "request", "user", "the request", agenttest.AssembleBase.Add(time.Second))
	oldAnswer := agenttest.AssembleText(t, "old-answer", "assistant", "the replaced answer", agenttest.AssembleBase.Add(2*time.Second))
	history.big = append(append([]messagepkg.Message(nil), visible...), request, oldAnswer)
	history.small = []messagepkg.Message{
		agenttest.AssembleText(t, "summary", "assistant", "<summary>short</summary>", agenttest.AssembleBase),
		request, oldAnswer,
	}
	runner.onRun = history.markCompacted
	req.HistoryCutoffBeforeMessageID = "old-answer"
	req.RequiredHistoryMessageID = "request"
	req.PersistedUserMessageID = "request"
	req.ReusePersistedUserMessage = true
	req.TurnReplacement = &messagepkg.TurnReplacement{}
	image := sdk.ImagePart{Image: "data:image/png;base64,AAAA"}
	run := &stepCompaction{
		service:          service,
		req:              req,
		requestMessageID: func() string { return "request" },
		budget:           syncTestBudget,
		replacement:      true,
		tailStart:        3,
		inlineParts:      []sdk.MessagePart{image},
	}

	got := run.compact(context.Background(), loopInput(replacementLoopMessages(image)))

	assertSDKTexts(t, got, "<summary>short</summary>", "the request", "calling lookup", "lookup result")
	if !requestCarriesImage(got[:2], image) {
		t.Fatalf("the retried request lost its image: %#v", got)
	}
	if run.tailStart != 2 {
		t.Fatalf("tailStart = %d, want the run's own messages to start after the request row", run.tailStart)
	}
}
