package wechatoa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/channeltest"
	"github.com/felinics/memoh/internal/channel/delivery"
)

// wechatPlatform stands in for the WeChat API: it records the customer service
// messages the adapter sends.
type wechatPlatform struct {
	adapter *WeChatOAAdapter
	cfg     channel.ChannelConfig

	mu      sync.Mutex
	sent    []map[string]any
	uploads int
}

func newWeChatPlatform(t *testing.T) *wechatPlatform {
	t.Helper()
	p := &wechatPlatform{adapter: NewWeChatOAAdapter(nil)}
	p.cfg = channel.ChannelConfig{
		ID:          "cfg-1",
		BotID:       "bot-1",
		ChannelType: Type,
		Credentials: map[string]any{"appId": "test-app", "appSecret": "app-secret", "token": "webhook-token", "encryptionMode": "plain"},
	}
	client, err := p.adapter.clientForConfig(p.cfg.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	client.http = &http.Client{Transport: tokenTestTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"access_token":"valid-token","expires_in":7200}`
		if strings.Contains(req.URL.Path, "/media/upload") {
			p.mu.Lock()
			p.uploads++
			body = fmt.Sprintf(`{"media_id":"media-%d"}`, p.uploads)
			p.mu.Unlock()
		}
		if strings.Contains(req.URL.Path, "/message/custom/send") {
			var payload map[string]any
			raw, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(raw, &payload)
			p.mu.Lock()
			p.sent = append(p.sent, payload)
			p.mu.Unlock()
			body = `{"errcode":0}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	return p
}

func (p *wechatPlatform) messages() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any{}, p.sent...)
}

func textOf(msg map[string]any) string {
	text, _ := msg["text"].(map[string]any)
	content, _ := text["content"].(string)
	return content
}

func (p *wechatPlatform) open(t *testing.T, receivedAt time.Time) *delivery.Reply {
	t.Helper()
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         replySender(t, p.adapter, p.cfg),
		Target:          "openid:user-1",
		Reply:           &channel.ReplyRef{Target: "openid:user-1", MessageID: "msg-1"},
		SourceMessageID: "msg-1",
		ReceivedAt:      receivedAt,
	})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	return reply
}

func TestDescriptorDeclaresNoPreviewAndTheCustomerServiceWindow(t *testing.T) {
	t.Parallel()
	caps := NewWeChatOAAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewNone {
		t.Fatalf("preview = %v, want none", caps.Preview)
	}
	if caps.ReplyWindow != 48*time.Hour || caps.ProactiveSend {
		t.Fatalf("reply window = %v, proactive send = %v, want 48h and false", caps.ReplyWindow, caps.ProactiveSend)
	}
	if _, ok := channel.Adapter(NewWeChatOAAdapter(nil)).(channel.PreviewOpener); ok {
		t.Fatal("an adapter that declares no preview opens none")
	}
}

// Nothing reaches the user while the run streams. The answer goes out once,
// when the run ends.
func TestReplyIsSentOnceWhenTheRunEnds(t *testing.T) {
	p := newWeChatPlatform(t)
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
	if got := p.messages(); len(got) != 0 {
		t.Fatalf("messages before the end = %+v, want none", got)
	}
	if err := reply.End(ctx, []channel.Message{{Text: "Hello"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	got := p.messages()
	if len(got) != 1 || textOf(got[0]) != "Hello" || got[0]["touser"] != "user-1" {
		t.Fatalf("messages = %+v, want the answer once, to the user", got)
	}
}

func TestReplyErrorIsAMessageOfItsOwn(t *testing.T) {
	p := newWeChatPlatform(t)
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
	got := p.messages()
	if len(got) != 1 || textOf(got[0]) != "The workspace is unreachable." {
		t.Fatalf("messages = %+v, want the error once and not the draft", got)
	}
}

// Each attachment is a message of its own, sent when the run produces it; the
// API takes one attachment per message.
func TestReplySendsEachAttachmentOnItsOwn(t *testing.T) {
	p := newWeChatPlatform(t)
	reply := p.open(t, time.Now())
	ctx := context.Background()

	image := base64.StdEncoding.EncodeToString([]byte("png"))
	err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventAttachment, Attachments: []channel.Attachment{
		{Type: channel.AttachmentImage, Base64: image, Mime: "image/png", Name: "a.png"},
		{Type: channel.AttachmentImage, Base64: image + "AA==", Mime: "image/png", Name: "b.png"},
	}})
	if err != nil {
		t.Fatalf("Show attachment: %v", err)
	}
	if err := reply.End(ctx, []channel.Message{{Text: "Here they are"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	got := p.messages()
	if len(got) != 3 {
		t.Fatalf("messages = %+v, want two images and the text", got)
	}
	for i, media := range []string{"media-1", "media-2"} {
		image, _ := got[i]["image"].(map[string]any)
		if got[i]["msgtype"] != "image" || image["media_id"] != media {
			t.Fatalf("message %d = %+v, want image %s", i, got[i], media)
		}
	}
	if textOf(got[2]) != "Here they are" {
		t.Fatalf("last message = %+v, want the text", got[2])
	}
}

// The customer service API only reaches a user who wrote in the last 48 hours,
// and the account cannot message them otherwise, so a later reply fails with
// the window's code instead of the platform's error.
func TestReplyWindowIs48Hours(t *testing.T) {
	p := newWeChatPlatform(t)
	ctx := context.Background()

	inside := p.open(t, time.Now().Add(-47*time.Hour))
	if err := inside.End(ctx, []channel.Message{{Text: "in time"}}, false); err != nil {
		t.Fatalf("End inside the window: %v", err)
	}
	if got := p.messages(); len(got) != 1 {
		t.Fatalf("messages = %+v, want the reply inside the window", got)
	}

	outside := p.open(t, time.Now().Add(-49*time.Hour))
	err := outside.End(ctx, []channel.Message{{Text: "too late"}}, false)
	if got := apperror.CodeOf(err); got != apperror.CodeChannelReplyWindowExpired {
		t.Fatalf("End after the window = %v (code %q), want %q", err, got, apperror.CodeChannelReplyWindowExpired)
	}
	if got := p.messages(); len(got) != 1 {
		t.Fatalf("messages = %+v, want nothing sent after the window", got)
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
	manager.SetAttachmentStore(channeltest.NewMemoryAttachmentStore())
	t.Cleanup(func() {
		close(capture.release)
		_ = manager.Shutdown(context.Background())
	})
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: cfg.BotID, Channel: cfg.ChannelType, ReplyTarget: "openid:user-1"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	return <-capture.sender
}
