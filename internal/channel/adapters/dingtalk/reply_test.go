package dingtalk

import (
	"context"
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

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/delivery"
)

// dingtalkPlatform stands in for DingTalk's webhook and OpenAPI endpoints and
// records what the adapter posts to them, in order.
type dingtalkPlatform struct {
	adapter *DingTalkAdapter
	cfg     channel.ChannelConfig
	url     string

	mu       sync.Mutex
	requests []platformRequest
}

type platformRequest struct {
	path string
	body map[string]any
}

func newDingTalkPlatform(t *testing.T) *dingtalkPlatform {
	t.Helper()
	p := &dingtalkPlatform{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		p.mu.Lock()
		p.requests = append(p.requests, platformRequest{path: r.URL.Path, body: body})
		p.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	p.url = server.URL

	p.adapter = NewDingTalkAdapter(nil)
	p.cfg = channel.ChannelConfig{
		ID:          "cfg-1",
		BotID:       "bot-1",
		ChannelType: Type,
		Credentials: map[string]any{"appKey": "key", "appSecret": "secret"},
	}
	p.adapter.apiClients[p.cfg.ID] = &apiClient{
		appKey: "key", appSecret: "secret", base: server.URL,
		token: "token", tokenExp: time.Now().Add(time.Hour), http: server.Client(),
	}
	p.adapter.rememberWebhook(p.cfg.ID, "source-1", sessionWebhookContext{
		SessionWebhook: server.URL + "/hook",
		ExpiredTime:    time.Now().Add(time.Hour).UnixMilli(),
	})
	return p
}

func (p *dingtalkPlatform) posted() []platformRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]platformRequest{}, p.requests...)
}

// webhookText is the text of a request to the session webhook.
func webhookText(t *testing.T, req platformRequest) string {
	t.Helper()
	if req.path != "/hook" {
		t.Fatalf("request path = %q, want the session webhook", req.path)
	}
	text, _ := req.body["text"].(map[string]any)
	content, _ := text["content"].(string)
	return content
}

var sourceReply = &channel.ReplyRef{Target: "user:alice", MessageID: "source-1"}

func (p *dingtalkPlatform) open(t *testing.T, receivedAt time.Time) *delivery.Reply {
	t.Helper()
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         replySender(t, p.adapter, p.cfg),
		Target:          "user:alice",
		Reply:           sourceReply,
		SourceMessageID: "source-1",
		ReceivedAt:      receivedAt,
	})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	return reply
}

func TestDescriptorDeclaresNoPreview(t *testing.T) {
	t.Parallel()
	caps := NewDingTalkAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewNone {
		t.Fatalf("preview = %v, want none", caps.Preview)
	}
	if caps.ReplyWindow != 0 {
		t.Fatalf("reply window = %v, want none: Send falls back to the OpenAPI", caps.ReplyWindow)
	}
	if _, ok := channel.Adapter(NewDingTalkAdapter(nil)).(channel.PreviewOpener); ok {
		t.Fatal("an adapter that declares no preview opens none")
	}
}

// Nothing reaches DingTalk while the run streams. The answer goes out once,
// through the session webhook of the message it answers.
func TestReplyIsSentOnceWhenTheRunEnds(t *testing.T) {
	p := newDingTalkPlatform(t)
	reply := p.open(t, time.Now())
	ctx := context.Background()

	if err := reply.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, text := range []string{"Hel", "lo"} {
		if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}); err != nil {
			t.Fatalf("Show: %v", err)
		}
	}
	if got := p.posted(); len(got) != 0 {
		t.Fatalf("requests before the end = %+v, want none", got)
	}
	if err := reply.End(ctx, []channel.Message{{Text: "Hello"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	got := p.posted()
	if len(got) != 1 || webhookText(t, got[0]) != "Hello" {
		t.Fatalf("requests = %+v, want the answer once", got)
	}
}

func TestReplySendsEachOutputAsAMessage(t *testing.T) {
	p := newDingTalkPlatform(t)
	reply := p.open(t, time.Now())

	if err := reply.End(context.Background(), []channel.Message{{Text: "first"}, {Text: "second"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	got := p.posted()
	if len(got) != 2 || webhookText(t, got[0]) != "first" || webhookText(t, got[1]) != "second" {
		t.Fatalf("requests = %+v, want both outputs in order", got)
	}
}

// With no preview, an error is a message of its own whether or not text came
// before it, and the run's end adds nothing.
func TestReplyErrorIsAMessageOfItsOwn(t *testing.T) {
	p := newDingTalkPlatform(t)
	reply := p.open(t, time.Now())
	ctx := context.Background()

	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "draft", Phase: channel.StreamPhaseText}); err != nil {
		t.Fatalf("Show delta: %v", err)
	}
	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"}); err != nil {
		t.Fatalf("Show error: %v", err)
	}
	if err := reply.End(ctx, nil, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	got := p.posted()
	if len(got) != 1 || webhookText(t, got[0]) != "The workspace is unreachable." {
		t.Fatalf("requests = %+v, want the error once and not the draft", got)
	}
}

// An attachment is a message of its own, sent when the run produces it and not
// bundled with the text. The session webhook carries no media, so it goes
// through the OpenAPI.
func TestReplySendsAnAttachmentOnItsOwn(t *testing.T) {
	p := newDingTalkPlatform(t)
	reply := p.open(t, time.Now())
	ctx := context.Background()

	err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{
		{Type: channel.AttachmentImage, URL: "https://example.com/a.png"},
	}})
	if err != nil {
		t.Fatalf("Show attachment: %v", err)
	}
	if err := reply.End(ctx, []channel.Message{{Text: "Here it is"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	got := p.posted()
	if len(got) != 2 {
		t.Fatalf("requests = %+v, want the image and then the text", got)
	}
	if got[0].path != "/v1.0/robot/oToMessages/batchSend" || got[0].body["msgKey"] != "sampleImageMsg" {
		t.Fatalf("first request = %+v, want the image through the OpenAPI", got[0])
	}
	if !strings.Contains(fmt.Sprint(got[0].body["msgParam"]), "https://example.com/a.png") {
		t.Fatalf("image request = %+v, want the picture's URL", got[0])
	}
	if webhookText(t, got[1]) != "Here it is" {
		t.Fatalf("second request = %+v, want the text", got[1])
	}
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
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: cfg.BotID, Channel: cfg.ChannelType, ReplyTarget: "user:alice"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	return <-capture.sender
}
