package wecom

import (
	"context"
	"crypto/md5" //nolint:gosec // test checks the protocol's image checksum.
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/delivery"
	"github.com/felinics/memoh/internal/redact"
)

var (
	_ channel.PreviewOpener = (*WeComAdapter)(nil)

	callbackReply = &channel.ReplyRef{Target: "chat_id:chat_1", MessageID: "msg_1"}
)

// wecomPlatform stands in for WeCom's long-connection endpoint: it accepts the
// bot's subscription and records every frame the adapter sends.
type wecomPlatform struct {
	t       *testing.T
	adapter *WeComAdapter
	cfg     channel.ChannelConfig

	mu     sync.Mutex
	frames []WSFrame
}

func newWeComPlatform(t *testing.T) *wecomPlatform {
	t.Helper()
	p := &wecomPlatform{t: t}
	var once sync.Once
	subscribed := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var subscribe WSFrame
		if conn.ReadJSON(&subscribe) != nil {
			return
		}
		_ = conn.WriteJSON(WSFrame{Headers: subscribe.Headers})
		once.Do(func() { close(subscribed) })
		for {
			var frame WSFrame
			if conn.ReadJSON(&frame) != nil {
				return
			}
			p.mu.Lock()
			p.frames = append(p.frames, frame)
			p.mu.Unlock()
			// Replies to a callback are not acknowledged; pushed messages are.
			if frame.Cmd == WSCmdSendMessage {
				_ = conn.WriteJSON(WSFrame{Headers: frame.Headers})
			}
		}
	}))
	t.Cleanup(server.Close)

	p.adapter = NewWeComAdapter(nil)
	p.cfg = channel.ChannelConfig{
		ID:          "cfg-1",
		BotID:       "bot-1",
		ChannelType: Type,
		Credentials: map[string]any{
			"botId":  "bot",
			"secret": "sec",
			"wsUrl":  "ws" + strings.TrimPrefix(server.URL, "http"),
		},
	}
	conn, err := p.adapter.Connect(context.Background(), p.cfg, func(context.Context, channel.ChannelConfig, channel.InboundMessage) error { return nil })
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Stop(context.Background()) })
	select {
	case <-subscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("the bot never subscribed")
	}
	return p
}

// callback records an inbound callback the bot can reply to.
func (p *wecomPlatform) callback(reqID string) {
	p.adapter.rememberCallback(p.cfg.ID, "msg_1", reqID, "", "chat_1", "u1")
}

// settle returns the frames the platform has received so far. A pushed message
// is acknowledged only after every frame sent before it was read, so the ack of
// a marker message means the replies are all in.
func (p *wecomPlatform) settle() []WSFrame {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := p.adapter.getClient(p.cfg.ID, "bot")
	if _, err := client.Reply(ctx, NewReqID("marker"), WSCmdSendMessage, SendMessageMarkdownBody{
		ChatID: "marker", MsgType: "markdown", Markdown: markdownPayload{Content: "marker"},
	}); err != nil {
		p.t.Fatalf("marker message: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var frames []WSFrame
	for _, frame := range p.frames {
		if frame.Cmd == WSCmdSendMessage && strings.Contains(string(frame.Body), `"marker"`) {
			continue
		}
		frames = append(frames, frame)
	}
	return frames
}

func (p *wecomPlatform) open(t *testing.T) channel.PreparedPreview {
	t.Helper()
	preview, err := p.adapter.OpenPreview(context.Background(), p.cfg, "chat_id:chat_1", channel.StreamOptions{Reply: callbackReply})
	if err != nil {
		t.Fatalf("OpenPreview: %v", err)
	}
	return preview
}

func streamOf(t *testing.T, frame WSFrame) StreamReplyBlock {
	t.Helper()
	if frame.Cmd != WSCmdRespond {
		t.Fatalf("frame cmd = %q, want %q", frame.Cmd, WSCmdRespond)
	}
	var body StreamReplyBody
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		t.Fatalf("decode stream body: %v", err)
	}
	return body.Stream
}

func pushedText(t *testing.T, frame WSFrame) string {
	t.Helper()
	if frame.Cmd != WSCmdSendMessage {
		t.Fatalf("frame cmd = %q, want %q", frame.Cmd, WSCmdSendMessage)
	}
	var body SendMessageMarkdownBody
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		t.Fatalf("decode pushed body: %v", err)
	}
	return body.Markdown.Content
}

func delta(text string) channel.PreparedStreamEvent {
	return channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}
}

func finalOf(text string) *channel.PreparedStreamEvent {
	return &channel.PreparedStreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.PreparedStreamFinalizePayload{Message: channel.PreparedMessage{
			Message: channel.Message{Text: text, Reply: callbackReply},
		}},
	}
}

func mustUpdate(t *testing.T, preview channel.PreparedPreview, event channel.PreparedStreamEvent) {
	t.Helper()
	if err := preview.Update(context.Background(), event); err != nil {
		t.Fatalf("Update(%s): %v", event.Type, err)
	}
}

func mustFinish(t *testing.T, preview channel.PreparedPreview, final *channel.PreparedStreamEvent) {
	t.Helper()
	if err := preview.Finish(context.Background(), final); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestDescriptorDeclaresNativeStream(t *testing.T) {
	t.Parallel()
	caps := NewWeComAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewNativeStream {
		t.Fatalf("preview = %v, want native stream", caps.Preview)
	}
	if caps.ReplyWindow != 24*time.Hour || !caps.ProactiveSend {
		t.Fatalf("reply window = %v, proactive send = %v, want 24h and true", caps.ReplyWindow, caps.ProactiveSend)
	}
}

func TestOpenPreviewRepliesToTheSourceMessage(t *testing.T) {
	t.Parallel()
	preview, err := NewWeComAdapter(nil).OpenPreview(context.Background(), channel.ChannelConfig{}, "chat_id:chat_1", channel.StreamOptions{SourceMessageID: "msg_1"})
	if err != nil {
		t.Fatalf("OpenPreview: %v", err)
	}
	reply := preview.(*wecomPreview).reply
	if reply == nil || reply.MessageID != "msg_1" || reply.Target != "chat_id:chat_1" {
		t.Fatalf("reply = %+v, want the source message as reply", reply)
	}
	if _, err := NewWeComAdapter(nil).OpenPreview(context.Background(), channel.ChannelConfig{}, " ", channel.StreamOptions{}); err == nil {
		t.Fatal("OpenPreview without a target succeeded")
	}
}

// The text so far is sent whole under one stream id on every refresh, and the
// final closes that stream.
func TestPreviewRefreshesOneStreamMessage(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted})
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: "thinking", Phase: channel.StreamPhaseReasoning})
	mustUpdate(t, preview, delta("Hel"))
	mustUpdate(t, preview, delta("lo"))
	mustUpdate(t, preview, delta("  "))
	mustFinish(t, preview, finalOf("Hello world"))

	frames := p.settle()
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 2 refreshes and the finish", len(frames))
	}
	want := []struct {
		text   string
		finish bool
	}{{"Hel", false}, {"Hello", false}, {"Hello world", true}}
	id := streamOf(t, frames[0]).ID
	for i, frame := range frames {
		stream := streamOf(t, frame)
		if stream.Content != want[i].text || stream.Finish != want[i].finish || stream.ID != id || id == "" {
			t.Fatalf("frame %d = %+v, want %q finish=%v under stream %q", i, stream, want[i].text, want[i].finish, id)
		}
		if frame.Headers.ReqID != "req_1" {
			t.Fatalf("frame %d req_id = %q, want the callback's", i, frame.Headers.ReqID)
		}
	}
}

func TestPreviewSkipsARefreshThatChangesNothing(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, delta("Hello"))
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: "\n", Phase: channel.StreamPhaseText})
	if frames := p.settle(); len(frames) != 1 {
		t.Fatalf("frames = %d, want the one refresh", len(frames))
	}
}

func TestPreviewFinishEndsTheReplyOnce(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, delta("Hello"))
	mustFinish(t, preview, finalOf("Hello"))
	mustFinish(t, preview, finalOf("again"))
	mustUpdate(t, preview, delta("late"))
	if err := preview.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	frames := p.settle()
	if len(frames) != 2 || !streamOf(t, frames[1]).Finish {
		t.Fatalf("frames = %+v, want the refresh and one finish", frames)
	}
}

// Text a stream has shown cannot be taken back: a reply that says nothing more
// closes the stream with that text, and one that shows nothing sends nothing.
func TestPreviewFinishWithoutAFinal(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")

	empty := p.open(t)
	mustFinish(t, empty, nil)
	if frames := p.settle(); len(frames) != 0 {
		t.Fatalf("a reply with nothing to show sent %+v", frames)
	}

	shown := p.open(t)
	mustUpdate(t, shown, delta("partial"))
	mustFinish(t, shown, nil)
	frames := p.settle()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the refresh and the close", len(frames))
	}
	closing := streamOf(t, frames[1])
	if !closing.Finish || closing.Content != "partial" || closing.ID != streamOf(t, frames[0]).ID {
		t.Fatalf("closing frame = %+v, want the stream closed with its text", closing)
	}
}

func TestPreviewFinishWithAnEmptyFinalClosesWithTheStreamedText(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, delta("first part"))
	mustFinish(t, preview, finalOf(""))
	frames := p.settle()
	if got := streamOf(t, frames[len(frames)-1]); !got.Finish || got.Content != "first part" {
		t.Fatalf("last frame = %+v, want the stream closed with its text", got)
	}
}

func TestPreviewResetReplacesTheText(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, delta("failed attempt"))
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventReset})
	mustUpdate(t, preview, delta("retry"))
	mustFinish(t, preview, nil)

	frames := p.settle()
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want two refreshes and the close", len(frames))
	}
	if got := streamOf(t, frames[2]); got.Content != "retry" || !got.Finish || got.ID != streamOf(t, frames[0]).ID {
		t.Fatalf("closing frame = %+v, want the retry text on the same stream", got)
	}
}

// With no text shown, an error is the reply's terminal state: it takes the
// place of the answer, and nothing that follows adds to it.
func TestPreviewErrorIsTheTerminalState(t *testing.T) {
	redact.ResetForTest()
	t.Cleanup(redact.ResetForTest)
	const secret = "wecom-secret-value-123456"
	redact.SetSecrets("wecom-preview-test", secret)

	cases := []struct {
		name  string
		event channel.PreparedStreamEvent
		want  string
	}{
		{
			name:  "coded error shows the copy as it is",
			event: channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"},
			want:  "The workspace is unreachable.",
		},
		{
			name:  "uncoded error is redacted and labelled",
			event: channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "request failed with token " + secret},
			want:  "Error: request failed with token " + strings.Repeat("*", len(secret)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newWeComPlatform(t)
			p.callback("req_1")
			preview := p.open(t)

			mustUpdate(t, preview, tc.event)
			mustFinish(t, preview, finalOf("an answer after the error"))
			mustUpdate(t, preview, delta("more"))
			if err := preview.Abort(context.Background()); err != nil {
				t.Fatalf("Abort: %v", err)
			}
			frames := p.settle()
			if len(frames) != 1 {
				t.Fatalf("frames = %+v, want only the error", frames)
			}
			if got := streamOf(t, frames[0]); got.Content != tc.want || !got.Finish {
				t.Fatalf("frame = %+v, want the error closing the stream", got)
			}
		})
	}
}

func TestPreviewBlankErrorIsNotATerminalState(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "  "})
	if frames := p.settle(); len(frames) != 0 {
		t.Fatalf("a blank error sent %+v", frames)
	}
	mustFinish(t, preview, finalOf("answer"))
	frames := p.settle()
	if len(frames) != 1 || streamOf(t, frames[0]).Content != "answer" {
		t.Fatalf("frames = %+v, want the answer", frames)
	}
}

// An error after a retry replaces the stale text on the same stream.
func TestPreviewErrorAfterResetReplacesTheStaleText(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	mustUpdate(t, preview, delta("stale"))
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventReset})
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventError, Error: "gone", ErrorCode: "run.failed"})
	frames := p.settle()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the refresh and the error", len(frames))
	}
	if got := streamOf(t, frames[1]); got.Content != "gone" || !got.Finish || got.ID != streamOf(t, frames[0]).ID {
		t.Fatalf("error frame = %+v, want it on the stream the stale text is in", got)
	}
}

func TestPreviewAbortClosesTheStreamWithItsText(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")

	untouched := p.open(t)
	if err := untouched.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if frames := p.settle(); len(frames) != 0 {
		t.Fatalf("aborting a reply with nothing shown sent %+v", frames)
	}

	preview := p.open(t)
	mustUpdate(t, preview, delta("partial"))
	if err := preview.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	mustUpdate(t, preview, delta("late"))
	mustFinish(t, preview, finalOf("late"))
	frames := p.settle()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the refresh and the close, nothing after Abort", len(frames))
	}
	if got := streamOf(t, frames[1]); got.Content != "partial" || !got.Finish {
		t.Fatalf("close frame = %+v, want the stream closed with its text", got)
	}
}

// A tool call's outcome is a message of its own; the stream message keeps the
// text.
func TestPreviewToolCallSummaryIsPushedAsAMessage(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	call := &channel.StreamToolCall{Name: "read_file", Input: map[string]any{"path": "a.txt"}, Result: map[string]any{"ok": true}}
	want := strings.TrimSpace(channel.RenderToolCallMessage(channel.BuildToolCallEnd(call)))
	if want == "" {
		t.Fatal("the tool call renders nothing")
	}
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: call})
	mustUpdate(t, preview, channel.PreparedStreamEvent{Type: channel.StreamEventToolCallEnd, ToolCall: call})

	frames := p.settle()
	if len(frames) != 1 || pushedText(t, frames[0]) != want {
		t.Fatalf("frames = %+v, want the summary pushed once", frames)
	}
}

// Without a callback to answer, no stream message can be refreshed. The text
// waits and is pushed whole when the reply ends.
func TestPreviewWithoutACallbackPushesAtTheEnd(t *testing.T) {
	p := newWeComPlatform(t)
	preview := p.open(t)

	mustUpdate(t, preview, delta("Hel"))
	mustUpdate(t, preview, delta("lo"))
	if frames := p.settle(); len(frames) != 0 {
		t.Fatalf("frames = %+v before the end, want none", frames)
	}
	mustFinish(t, preview, nil)
	frames := p.settle()
	if len(frames) != 1 || pushedText(t, frames[0]) != "Hello" {
		t.Fatalf("frames = %+v, want the text pushed once", frames)
	}
}

func TestPreviewFinishCarriesAnImage(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	preview := p.open(t)

	png := []byte("not really a png")
	final := finalOf("here it is")
	att := imageAttachment(png)
	final.Final.Message.Attachments = []channel.PreparedAttachment{att}
	final.Final.Message.Message.Attachments = []channel.Attachment{att.Logical}
	mustFinish(t, preview, final)

	frames := p.settle()
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want one", len(frames))
	}
	stream := streamOf(t, frames[0])
	if stream.Content != "here it is" || len(stream.MsgItems) != 1 || stream.MsgItems[0].Image == nil {
		t.Fatalf("stream = %+v, want the text with one image", stream)
	}
	if got := stream.MsgItems[0].Image; got.Base64 != base64.StdEncoding.EncodeToString(png) || got.MD5 != fmt.Sprintf("%x", md5.Sum(png)) { //nolint:gosec // protocol checksum.
		t.Fatalf("image = %+v, want the bytes and their md5", got)
	}
}

func imageAttachment(data []byte) channel.PreparedAttachment {
	return channel.PreparedAttachment{
		Kind:    channel.PreparedAttachmentUpload,
		Logical: channel.Attachment{Type: channel.AttachmentImage},
		Mime:    "image/png",
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(string(data))), nil
		},
	}
}

// aibot_send_msg carries no media. An attachment arrives without the reply
// reference of its message, so it answers the newest callback of its
// conversation.
func TestSendAttachmentAnswersTheNewestCallbackOfTheConversation(t *testing.T) {
	p := newWeComPlatform(t)
	p.adapter.rememberCallback(p.cfg.ID, "msg_0", "req_old", "", "chat_1", "u1")
	p.adapter.rememberCallback(p.cfg.ID, "msg_1", "req_new", "", "chat_1", "u1")
	p.adapter.rememberCallback(p.cfg.ID, "msg_2", "req_other", "", "chat_2", "u1")
	att := imageAttachment([]byte("image"))

	err := p.adapter.Send(context.Background(), p.cfg, channel.PreparedOutboundMessage{
		Target:  "chat_id:chat_1",
		Message: channel.PreparedMessage{Message: channel.Message{Attachments: []channel.Attachment{att.Logical}}, Attachments: []channel.PreparedAttachment{att}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	frames := p.settle()
	if len(frames) != 1 || frames[0].Headers.ReqID != "req_new" || len(streamOf(t, frames[0]).MsgItems) != 1 {
		t.Fatalf("frames = %+v, want the image answering the newest callback of chat_1", frames)
	}
}

func TestSendAttachmentWithoutACallbackFails(t *testing.T) {
	p := newWeComPlatform(t)
	att := imageAttachment([]byte("image"))
	err := p.adapter.Send(context.Background(), p.cfg, channel.PreparedOutboundMessage{
		Target:  "chat_id:chat_1",
		Message: channel.PreparedMessage{Message: channel.Message{Attachments: []channel.Attachment{att.Logical}}, Attachments: []channel.PreparedAttachment{att}},
	})
	if err == nil {
		t.Fatal("Send of an attachment with no callback to answer succeeded")
	}
}

// replySender returns the reply sender a real manager makes for the adapter, so
// a reply is routed as it is in production.
func replySender(t *testing.T, adapter channel.Adapter, cfg channel.ChannelConfig) channel.StreamReplySender {
	t.Helper()
	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	capture := &captureProcessor{sender: make(chan channel.StreamReplySender, 1), release: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, singleConfigStore{cfg: cfg}, capture)
	t.Cleanup(func() {
		close(capture.release)
		_ = manager.Shutdown(context.Background())
	})
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: cfg.BotID, Channel: cfg.ChannelType, ReplyTarget: "chat_id:chat_1"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	return <-capture.sender
}

type singleConfigStore struct{ cfg channel.ChannelConfig }

func (s singleConfigStore) ListConfigsByType(context.Context, channel.ChannelType) ([]channel.ChannelConfig, error) {
	return []channel.ChannelConfig{s.cfg}, nil
}

func (s singleConfigStore) ResolveEffectiveConfig(context.Context, string, channel.ChannelType) (channel.ChannelConfig, error) {
	return s.cfg, nil
}

func (singleConfigStore) GetChannelIdentityConfig(context.Context, string, channel.ChannelType) (channel.ChannelIdentityBinding, error) {
	return channel.ChannelIdentityBinding{}, errors.New("not used")
}

type captureProcessor struct {
	sender  chan channel.StreamReplySender
	release chan struct{}
}

func (c *captureProcessor) HandleInbound(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, sender channel.StreamReplySender) error {
	c.sender <- sender
	<-c.release
	return nil
}

func openReply(t *testing.T, p *wecomPlatform, receivedAt time.Time) *delivery.Reply {
	t.Helper()
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         replySender(t, p.adapter, p.cfg),
		Target:          "chat_id:chat_1",
		Reply:           callbackReply,
		SourceMessageID: "msg_1",
		ReceivedAt:      receivedAt,
	})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	return reply
}

// The mapping layer drives the adapter's preview: refreshes while the run
// streams, the first output as the stream's end, later outputs as messages.
func TestReplyThroughTheManagerStreamsAndFinishes(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	reply := openReply(t, p, time.Now())
	ctx := context.Background()

	if err := reply.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, text := range []string{"Hel", "lo"} {
		if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}); err != nil {
			t.Fatalf("Show: %v", err)
		}
	}
	if err := reply.End(ctx, []channel.Message{{Text: "Hello"}, {Text: "and more"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}

	frames := p.settle()
	if len(frames) != 4 {
		t.Fatalf("frames = %d, want two refreshes, the finish and one more message", len(frames))
	}
	first := streamOf(t, frames[0])
	if last := streamOf(t, frames[2]); last.Content != "Hello" || !last.Finish || last.ID != first.ID {
		t.Fatalf("finish frame = %+v, want the first output closing the stream", last)
	}
	if extra := streamOf(t, frames[3]); extra.Content != "and more" || extra.ID == first.ID {
		t.Fatalf("second output = %+v, want a stream message of its own", extra)
	}
}

func TestReplyThroughTheManagerEndsAStreamWithoutText(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	reply := openReply(t, p, time.Now())
	ctx := context.Background()

	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "partial", Phase: channel.StreamPhaseText}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if err := reply.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	frames := p.settle()
	if len(frames) != 2 || !streamOf(t, frames[1]).Finish || streamOf(t, frames[1]).Content != "partial" {
		t.Fatalf("frames = %+v, want the stream closed with its text", frames)
	}
}

func TestReplyThroughTheManagerShowsAnErrorInPlaceOfTheAnswer(t *testing.T) {
	p := newWeComPlatform(t)
	p.callback("req_1")
	reply := openReply(t, p, time.Now())
	ctx := context.Background()

	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventError, Error: "The run failed.", ErrorCode: "run.failed"}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if err := reply.End(ctx, nil, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	frames := p.settle()
	if len(frames) != 1 {
		t.Fatalf("frames = %+v, want only the error", frames)
	}
	if got := streamOf(t, frames[0]); got.Content != "The run failed." || !got.Finish {
		t.Fatalf("frame = %+v, want the error closing the stream", got)
	}
}

// After the window the reply is pushed without the callback it answers: the
// platform accepts that, so the reply neither fails nor goes to a callback
// WeCom no longer takes replies for.
func TestReplyThroughTheManagerPushesAfterTheWindow(t *testing.T) {
	p := newWeComPlatform(t)
	stale := time.Now().Add(-25 * time.Hour)
	p.adapter.cache.Put(p.cfg.ID, "msg_1", callbackContext{ReqID: "req_1", ChatID: "chat_1", UserID: "u1", CreatedAt: stale})
	reply := openReply(t, p, stale)
	ctx := context.Background()

	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "late", Phase: channel.StreamPhaseText}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if err := reply.End(ctx, []channel.Message{{Text: "late answer"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	frames := p.settle()
	if len(frames) != 1 || pushedText(t, frames[0]) != "late answer" {
		t.Fatalf("frames = %+v, want the answer pushed once", frames)
	}
}
