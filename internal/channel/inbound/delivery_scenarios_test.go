package inbound

// Scenarios for the reply a channel user sees. Each one runs a scripted turn
// through ChannelInboundProcessor.HandleInbound with a real channel.Manager
// reply sender over a recording adapter, so the recording is what an adapter
// receives below the manager's chunking and stream splitting.
//
// The same scenario definitions were run on the code before the delivery layer
// (4b97f4d95) and recorded into ../delivery/testdata/baseline.
// TestDeliveryMatchesBaseline replays them.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/identities"
	"github.com/felinics/memoh/internal/channel/route"
)

type recordedAttachment struct {
	Type string `json:"type,omitempty"`
	Kind string `json:"kind,omitempty"`
	Ref  string `json:"ref,omitempty"`
	Name string `json:"name,omitempty"`
}

type recordedMessage struct {
	Text        string               `json:"text,omitempty"`
	Format      string               `json:"format,omitempty"`
	ReplyTo     string               `json:"reply_to,omitempty"`
	Attachments []recordedAttachment `json:"attachments,omitempty"`
	Actions     []string             `json:"actions,omitempty"`
}

type recordedTool struct {
	Name       string   `json:"name,omitempty"`
	CallID     string   `json:"call_id,omitempty"`
	ApprovalID string   `json:"approval_id,omitempty"`
	ShortID    int      `json:"short_id,omitempty"`
	Actions    []string `json:"actions,omitempty"`
	Locale     string   `json:"locale,omitempty"`
	Input      string   `json:"input,omitempty"`
	Result     string   `json:"result,omitempty"`
}

type recordedEvent struct {
	Type        string               `json:"type"`
	Status      string               `json:"status,omitempty"`
	Delta       string               `json:"delta,omitempty"`
	Phase       string               `json:"phase,omitempty"`
	Error       string               `json:"error,omitempty"`
	ErrorCode   string               `json:"error_code,omitempty"`
	Tool        *recordedTool        `json:"tool,omitempty"`
	Final       *recordedMessage     `json:"final,omitempty"`
	Attachments []recordedAttachment `json:"attachments,omitempty"`
}

// recordedCall is one call the adapter received.
type recordedCall struct {
	Op        string           `json:"op"`
	Stream    int              `json:"stream,omitempty"`
	Target    string           `json:"target,omitempty"`
	ReplyTo   string           `json:"reply_to,omitempty"`
	Event     *recordedEvent   `json:"event,omitempty"`
	Message   *recordedMessage `json:"message,omitempty"`
	MessageID string           `json:"message_id,omitempty"`
	Emoji     string           `json:"emoji,omitempty"`
}

// deliveryRecording is the outcome of one scenario: the adapter's calls, what
// the stream observer saw, and the error HandleInbound returned.
type deliveryRecording struct {
	Name     string          `json:"name"`
	Err      string          `json:"err,omitempty"`
	Calls    []recordedCall  `json:"calls"`
	Observed []recordedEvent `json:"observed"`
}

func recordAttachments(items []channel.PreparedAttachment) []recordedAttachment {
	var out []recordedAttachment
	for _, item := range items {
		ref := item.NativeRef
		if ref == "" {
			ref = item.PublicURL
		}
		out = append(out, recordedAttachment{Type: string(item.Logical.Type), Kind: string(item.Kind), Ref: ref, Name: item.Name})
	}
	return out
}

func recordLogicalAttachments(items []channel.Attachment) []recordedAttachment {
	var out []recordedAttachment
	for _, item := range items {
		out = append(out, recordedAttachment{Type: string(item.Type), Ref: item.URL, Name: item.Name})
	}
	return out
}

func recordActions(actions []channel.Action) []string {
	var out []string
	for _, action := range actions {
		out = append(out, action.Type+":"+action.Value)
	}
	return out
}

func recordJSON(value any) string {
	if value == nil {
		return ""
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

func recordTool(tc *channel.StreamToolCall) *recordedTool {
	if tc == nil {
		return nil
	}
	return &recordedTool{
		Name: tc.Name, CallID: tc.CallID, ApprovalID: tc.ApprovalID, ShortID: tc.ShortID,
		Actions: recordActions(tc.Actions), Locale: tc.Locale, Input: recordJSON(tc.Input), Result: recordJSON(tc.Result),
	}
}

func recordPreparedMessage(msg channel.PreparedMessage) *recordedMessage {
	reply := ""
	if msg.Message.Reply != nil {
		reply = msg.Message.Reply.MessageID
	}
	return &recordedMessage{
		Text: msg.Message.Text, Format: string(msg.Message.Format), ReplyTo: reply,
		Attachments: recordAttachments(msg.Attachments), Actions: recordActions(msg.Message.Actions),
	}
}

func recordLogicalMessage(msg channel.Message) *recordedMessage {
	reply := ""
	if msg.Reply != nil {
		reply = msg.Reply.MessageID
	}
	return &recordedMessage{
		Text: msg.Text, Format: string(msg.Format), ReplyTo: reply,
		Attachments: recordLogicalAttachments(msg.Attachments), Actions: recordActions(msg.Actions),
	}
}

func recordPreparedEvent(event channel.PreparedStreamEvent) *recordedEvent {
	rec := &recordedEvent{
		Type: string(event.Type), Status: string(event.Status), Delta: event.Delta, Phase: string(event.Phase),
		Error: event.Error, ErrorCode: event.ErrorCode, Tool: recordTool(event.ToolCall),
		Attachments: recordAttachments(event.Attachments),
	}
	if event.Final != nil {
		rec.Final = recordPreparedMessage(event.Final.Message)
	}
	return rec
}

func recordLogicalEvent(event channel.StreamEvent) recordedEvent {
	rec := recordedEvent{
		Type: string(event.Type), Status: string(event.Status), Delta: event.Delta, Phase: string(event.Phase),
		Error: event.Error, ErrorCode: event.ErrorCode, Tool: recordTool(event.ToolCall),
		Attachments: recordLogicalAttachments(event.Attachments),
	}
	if event.Final != nil {
		rec.Final = recordLogicalMessage(event.Final.Message)
	}
	return rec
}

// recordingAdapter is a channel adapter that records what the manager hands it.
type recordingAdapter struct {
	typ    channel.ChannelType
	caps   channel.ChannelCapabilities
	policy channel.OutboundPolicy

	mu      sync.Mutex
	calls   []recordedCall
	streams int
}

func (a *recordingAdapter) Type() channel.ChannelType { return a.typ }

func (a *recordingAdapter) Descriptor() channel.Descriptor {
	return channel.Descriptor{Type: a.typ, DisplayName: "recording", Capabilities: a.caps, OutboundPolicy: a.policy}
}

func (a *recordingAdapter) record(call recordedCall) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
}

func (a *recordingAdapter) Send(_ context.Context, _ channel.ChannelConfig, msg channel.PreparedOutboundMessage) error {
	a.record(recordedCall{Op: "send", Target: msg.Target, Message: recordPreparedMessage(msg.Message)})
	return nil
}

func (a *recordingAdapter) OpenStream(_ context.Context, _ channel.ChannelConfig, target string, opts channel.StreamOptions) (channel.PreparedOutboundStream, error) {
	a.mu.Lock()
	a.streams++
	id := a.streams
	a.mu.Unlock()
	reply := ""
	if opts.Reply != nil {
		reply = opts.Reply.MessageID
	}
	a.record(recordedCall{Op: "open", Stream: id, Target: target, ReplyTo: reply})
	return &recordingStream{adapter: a, id: id}, nil
}

func (a *recordingAdapter) React(_ context.Context, _ channel.ChannelConfig, target, messageID, emoji string) error {
	a.record(recordedCall{Op: "react", Target: target, MessageID: messageID, Emoji: emoji})
	return nil
}

func (a *recordingAdapter) Unreact(_ context.Context, _ channel.ChannelConfig, target, messageID, emoji string) error {
	a.record(recordedCall{Op: "unreact", Target: target, MessageID: messageID, Emoji: emoji})
	return nil
}

type recordingStream struct {
	adapter *recordingAdapter
	id      int
}

func (s *recordingStream) Push(_ context.Context, event channel.PreparedStreamEvent) error {
	s.adapter.record(recordedCall{Op: "push", Stream: s.id, Event: recordPreparedEvent(event)})
	return nil
}

func (s *recordingStream) Close(context.Context) error {
	s.adapter.record(recordedCall{Op: "close", Stream: s.id})
	return nil
}

type recordingObserver struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (o *recordingObserver) OnStreamEvent(_ context.Context, _ string, _ channel.ChannelType, event channel.StreamEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, recordLogicalEvent(event))
}

type harnessStore struct{ cfg channel.ChannelConfig }

func (s *harnessStore) ListConfigsByType(context.Context, channel.ChannelType) ([]channel.ChannelConfig, error) {
	return []channel.ChannelConfig{s.cfg}, nil
}

func (s *harnessStore) ResolveEffectiveConfig(context.Context, string, channel.ChannelType) (channel.ChannelConfig, error) {
	return s.cfg, nil
}

func (*harnessStore) GetChannelIdentityConfig(context.Context, string, channel.ChannelType) (channel.ChannelIdentityBinding, error) {
	return channel.ChannelIdentityBinding{}, errors.New("not used")
}

// senderCapture is the processor the manager calls: it hands the manager's
// reply sender to the test and waits.
type senderCapture struct {
	sender chan channel.StreamReplySender
	done   chan struct{}
}

func (c *senderCapture) HandleInbound(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, sender channel.StreamReplySender) error {
	c.sender <- sender
	<-c.done
	return nil
}

type showToolCalls bool

func (s showToolCalls) ShowToolCallsInIM(context.Context, string) (bool, error) { return bool(s), nil }

type deliveryScenario struct {
	name  string
	caps  channel.ChannelCapabilities
	limit int
	tools bool
	// gateway builds the scripted run.
	gateway func() turn.Service
}

func scenarioCaps(mutate func(*channel.ChannelCapabilities)) channel.ChannelCapabilities {
	caps := channel.ChannelCapabilities{
		Text: true, Markdown: true, Attachments: true, Media: true, Reactions: true,
		Buttons: true, Reply: true, Streaming: true,
	}
	if mutate != nil {
		mutate(&caps)
	}
	return caps
}

func scriptedRun(payloads ...string) func() turn.Service {
	return func() turn.Service { return &scriptedFailureGateway{payloads: payloads} }
}

func scriptedRunErr(err error, payloads ...string) func() turn.Service {
	return func() turn.Service { return &scriptedFailureGateway{payloads: payloads, tailErr: err} }
}

func terminalRun(err error, payloads ...string) func() turn.Service {
	return func() turn.Service {
		return &terminalFailureGateway{scriptedFailureGateway: scriptedFailureGateway{payloads: payloads, tailErr: err}}
	}
}

func textDelta(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "text_delta", "delta": text})
	return string(raw)
}

func reasoningDelta(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "reasoning_delta", "delta": text})
	return string(raw)
}

func agentEnd(messages ...turn.ModelMessage) string {
	raw, _ := json.Marshal(map[string]any{"type": "agent_end", "messages": messages})
	return string(raw)
}

func assistantText(text string) turn.ModelMessage {
	return turn.ModelMessage{Role: "assistant", Content: turn.NewTextContent(text)}
}

func sendToolCall(text string) turn.ModelMessage {
	args, _ := json.Marshal(map[string]any{"text": text})
	return turn.ModelMessage{Role: "assistant", ToolCalls: []turn.ToolCall{{
		ID: "call-send", Type: "function", Function: turn.ToolCallFunction{Name: "send", Arguments: string(args)},
	}}}
}

const (
	approvalEvent   = `{"type":"tool_approval_request","toolName":"exec","toolCallId":"call-1","approvalId":"ap-1","shortId":3,"input":{"command":"ls"}}`
	askUserEvent    = `{"type":"user_input_request","toolName":"ask_user","toolCallId":"call-2","userInputId":"ui-1","shortId":4,"status":"pending","input":{"questions":[{"id":"q1","text":"Which one?"}]}}`
	toolStart       = `{"type":"tool_call_start","toolName":"search","toolCallId":"call-3","input":{"q":"memoh"}}`
	toolEnd         = `{"type":"tool_call_end","toolName":"search","toolCallId":"call-3","input":{"q":"memoh"},"result":{"hits":2}}`
	attachmentEvent = `{"type":"attachment_delta","attachments":[{"type":"image","url":"https://example.test/a.png","name":"a.png"}]}`
	reaction        = `{"type":"reaction_delta","reactions":[{"emoji":"👍"}]}`
	errorEvent      = `{"type":"error","code":"agent.provider_overloaded","error":"upstream text"}`
)

const longText = "first paragraph line one\nfirst paragraph line two\nsecond paragraph line one\nsecond paragraph line two\nthird paragraph line one"

func deliveryScenarios() []deliveryScenario {
	plain := scenarioCaps(nil)
	nativeApproval := scenarioCaps(func(c *channel.ChannelCapabilities) { c.NativeApproval = true })
	nativeInput := scenarioCaps(func(c *channel.ChannelCapabilities) { c.NativeUserInput = true })
	boom := errors.New("boom")
	return []deliveryScenario{
		{name: "normal_stream", caps: plain, gateway: scriptedRun(textDelta("hello "), textDelta("world"), agentEnd(assistantText("hello world")))},
		{name: "normal_final_only", caps: plain, gateway: scriptedRun(agentEnd(assistantText("hello world")))},
		{name: "silent_reply", caps: plain, gateway: scriptedRun(textDelta("NO_REPLY"), agentEnd(assistantText("NO_REPLY")))},
		{name: "reasoning_then_text", caps: plain, gateway: scriptedRun(
			`{"type":"reasoning_start"}`, reasoningDelta("thinking"), `{"type":"reasoning_end"}`,
			textDelta("answer"), agentEnd(assistantText("answer")))},

		{name: "error_before_text", caps: plain, gateway: scriptedRunErr(boom, `{"type":"agent_start"}`)},
		{name: "error_after_text", caps: plain, gateway: scriptedRunErr(boom, textDelta("partial"))},
		{name: "error_reasoning_only", caps: plain, gateway: scriptedRunErr(boom, `{"type":"reasoning_start"}`, reasoningDelta("thinking"))},
		{name: "error_event_before_text", caps: plain, gateway: scriptedRun(errorEvent, agentEnd())},
		{name: "error_event_after_text", caps: plain, gateway: scriptedRun(textDelta("partial"), errorEvent, agentEnd())},
		{name: "error_event_blank", caps: plain, gateway: scriptedRun(`{"type":"error"}`, agentEnd(assistantText("fine")))},
		{name: "start_error", caps: plain, gateway: func() turn.Service {
			return &scriptedFailureGateway{fakeChatGateway: fakeChatGateway{startErr: boom}}
		}},
		{name: "terminal_failed_after_text", caps: plain, gateway: terminalRun(boom,
			textDelta("partial"), errorEvent, `{"type":"run_terminal","state":"failed","error_code":"agent.provider_overloaded"}`)},
		{name: "terminal_failed_before_text", caps: plain, gateway: terminalRun(nil,
			errorEvent, `{"type":"run_terminal","state":"failed","error_code":"agent.provider_overloaded"}`)},
		{name: "terminal_recovered", caps: plain, gateway: terminalRun(nil,
			errorEvent, textDelta("answer"), agentEnd(assistantText("answer")), `{"type":"run_terminal","state":"completed"}`)},
		{name: "terminal_without_event", caps: plain, gateway: terminalRun(nil, errorEvent, `{"type":"agent_end","messages":[]}`)},

		{name: "approval_native", caps: nativeApproval, gateway: scriptedRun(textDelta("checking "), approvalEvent, textDelta("done"), agentEnd(assistantText("checking done")))},
		{name: "approval_text_after_text", caps: plain, gateway: scriptedRun(textDelta("checking "), approvalEvent, textDelta("done"), agentEnd(assistantText("checking done")))},
		{name: "approval_text_no_text", caps: plain, gateway: scriptedRun(approvalEvent, textDelta("done"), agentEnd(assistantText("done")))},
		{name: "approval_text_only", caps: plain, gateway: scriptedRun(textDelta("checking "), approvalEvent)},
		{name: "askuser_native", caps: nativeInput, gateway: scriptedRun(textDelta("one question "), askUserEvent, agentEnd(assistantText("one question")))},
		{name: "askuser_text_after_text", caps: plain, gateway: scriptedRun(textDelta("one question "), askUserEvent, textDelta("thanks"), agentEnd(assistantText("one question thanks")))},
		{name: "askuser_text_no_text", caps: plain, gateway: scriptedRun(askUserEvent, agentEnd())},
		{name: "approval_text_twice", caps: plain, gateway: scriptedRun(textDelta("a "), approvalEvent, textDelta("b "), askUserEvent, textDelta("c"), agentEnd(assistantText("a b c")))},

		{name: "tool_status_shown", caps: plain, tools: true, gateway: scriptedRun(toolStart, toolEnd, textDelta("found"), agentEnd(assistantText("found")))},
		{name: "tool_status_hidden", caps: plain, gateway: scriptedRun(toolStart, toolEnd, textDelta("found"), agentEnd(assistantText("found")))},
		{name: "approval_hidden_tools", caps: nativeApproval, gateway: scriptedRun(toolStart, approvalEvent, toolEnd, agentEnd(assistantText("ok")))},

		{name: "attachment_event", caps: plain, gateway: scriptedRun(attachmentEvent, textDelta("see attached"), agentEnd(assistantText("see attached")))},
		{name: "attachment_only", caps: plain, gateway: scriptedRun(attachmentEvent, agentEnd())},
		{name: "reaction_event", caps: plain, gateway: scriptedRun(reaction, textDelta("liked"), agentEnd(assistantText("liked")))},

		{name: "suppress_tail", caps: plain, gateway: scriptedRun(textDelta("sending "), agentEnd(sendToolCall("sending the report"), assistantText("sending the report")))},
		{name: "suppress_no_text", caps: plain, gateway: scriptedRun(agentEnd(sendToolCall("sending the report"), assistantText("sending the report")))},
		{name: "multi_assistant", caps: plain, gateway: scriptedRun(textDelta("first"), agentEnd(assistantText("first"), assistantText("second"), assistantText("third")))},
		{name: "multi_assistant_with_tool", caps: plain, gateway: scriptedRun(agentEnd(assistantText("first"), sendToolCallOther(), assistantText("second")))},

		{name: "long_final", caps: plain, limit: 60, gateway: scriptedRun(agentEnd(assistantText(longText)))},
		{name: "long_final_after_stream", caps: plain, limit: 60, gateway: scriptedRun(textDelta("first paragraph"), agentEnd(assistantText(longText)))},
		{name: "long_stream", caps: plain, limit: 40, gateway: scriptedRun(
			textDelta("first paragraph line one.\n"), textDelta("first paragraph line two.\n"), textDelta("second paragraph line one.\n"),
			agentEnd(assistantText("first paragraph line one.\nfirst paragraph line two.\nsecond paragraph line one.")))},
		{name: "long_stream_then_error", caps: plain, limit: 40, gateway: scriptedRunErr(boom,
			textDelta("first paragraph line one.\n"), textDelta("first paragraph line two.\n"), textDelta("second paragraph line one.\n"))},
	}
}

// sendToolCallOther is an assistant tool call that is not a send to the reply
// target, so it neither suppresses replies nor is an output.
func sendToolCallOther() turn.ModelMessage {
	return turn.ModelMessage{Role: "assistant", ToolCalls: []turn.ToolCall{{
		ID: "call-x", Type: "function", Function: turn.ToolCallFunction{Name: "search", Arguments: `{"q":"x"}`},
	}}}
}

func runDeliveryScenario(t *testing.T, sc deliveryScenario) deliveryRecording {
	t.Helper()
	adapter := &recordingAdapter{typ: channel.ChannelTypeTelegram, caps: sc.caps}
	if sc.limit > 0 {
		adapter.policy = channel.OutboundPolicy{TextChunkLimit: sc.limit}
	}
	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	cfg := channel.ChannelConfig{TeamID: "team-test", ID: "cfg-1", BotID: "bot-1", ChannelType: adapter.typ}
	capture := &senderCapture{sender: make(chan channel.StreamReplySender, 1), done: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, &harnessStore{cfg: cfg}, capture)
	defer func() {
		_ = manager.Shutdown(context.Background())
	}()

	routes := &fakeChatService{resolveResult: route.ResolveConversationResult{BotID: "chat-1", RouteID: "route-1"}}
	processor := NewChannelInboundProcessor(slog.New(slog.DiscardHandler), registry, routes, routes, sc.gateway(),
		&fakeChannelIdentityService{channelIdentity: identities.ChannelIdentity{ID: "identity-1"}}, &fakePolicyService{}, "", 0)
	observer := &recordingObserver{}
	processor.SetStreamObserver(observer)
	processor.SetReactor(manager)
	processor.SetIMDisplayOptions(showToolCalls(sc.tools))

	msg := channel.InboundMessage{
		BotID: "bot-1", Channel: adapter.typ, ReplyTarget: "target-id",
		Message: channel.Message{ID: "src-1", Text: "hello"},
		Sender:  channel.Identity{SubjectID: "ext-1"}, Conversation: channel.Conversation{ID: "chat-1", Type: channel.ConversationTypePrivate},
	}
	if err := manager.HandleInbound(context.Background(), cfg, msg); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	sender := <-capture.sender
	err := processor.HandleInbound(context.Background(), cfg, msg, sender)
	close(capture.done)

	rec := deliveryRecording{Name: sc.name}
	if err != nil {
		rec.Err = "error"
		if code := apperror.CodeOf(err); code != "" {
			rec.Err = string(code)
		}
	}
	adapter.mu.Lock()
	rec.Calls = append(rec.Calls, adapter.calls...)
	adapter.mu.Unlock()
	observer.mu.Lock()
	rec.Observed = append(rec.Observed, observer.events...)
	observer.mu.Unlock()
	return rec
}
