package weixin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/delivery"
)

// weixinPlatform stands in for the iLink API: it records the texts the adapter
// sends.
type weixinPlatform struct {
	adapter *WeixinAdapter
	cfg     channel.ChannelConfig

	mu    sync.Mutex
	texts []string
}

func newWeixinPlatform(t *testing.T) *weixinPlatform {
	t.Helper()
	p := &weixinPlatform{adapter: NewWeixinAdapter(nil)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SendMessageRequest
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		p.mu.Lock()
		for _, item := range req.Msg.ItemList {
			if item.TextItem != nil {
				p.texts = append(p.texts, item.TextItem.Text)
			}
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	t.Cleanup(server.Close)
	p.cfg = channel.ChannelConfig{
		ID:          "cfg-1",
		BotID:       "bot-1",
		ChannelType: Type,
		Credentials: map[string]any{"token": "bot-token", "baseUrl": server.URL},
	}
	p.adapter.contextCache.Put(p.cfg.ID+":user-1", "context-token")
	return p
}

func (p *weixinPlatform) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.texts...)
}

func (p *weixinPlatform) open(t *testing.T, receivedAt time.Time) *delivery.Reply {
	t.Helper()
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         replySender(t, p.adapter, p.cfg),
		Target:          "user-1",
		Reply:           &channel.ReplyRef{Target: "user-1", MessageID: "msg-1"},
		SourceMessageID: "msg-1",
		ReceivedAt:      receivedAt,
	})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	return reply
}

func TestDescriptorDeclaresNoPreviewAndTheContextTokenWindow(t *testing.T) {
	t.Parallel()
	adapter := NewWeixinAdapter(nil)
	caps := adapter.Descriptor().Capabilities
	if caps.Preview != channel.PreviewNone {
		t.Fatalf("preview = %v, want none", caps.Preview)
	}
	if caps.ReplyWindow != adapter.contextCache.ttl || caps.ReplyWindow != 24*time.Hour {
		t.Fatalf("reply window = %v, cache ttl = %v, want both 24h", caps.ReplyWindow, adapter.contextCache.ttl)
	}
	if caps.ProactiveSend {
		t.Fatal("every send needs a cached context_token, so there is no proactive send")
	}
	if _, ok := channel.Adapter(adapter).(channel.PreviewOpener); ok {
		t.Fatal("an adapter that declares no preview opens none")
	}
}

// Nothing reaches the user while the run streams. The answer goes out once,
// when the run ends.
func TestReplyIsSentOnceWhenTheRunEnds(t *testing.T) {
	p := newWeixinPlatform(t)
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
	if got := p.sent(); len(got) != 0 {
		t.Fatalf("texts before the end = %q, want none", got)
	}
	if err := reply.End(ctx, []channel.Message{{Text: "Hello"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	if got := p.sent(); len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("texts = %q, want the answer once", got)
	}
}

func TestReplyErrorIsAMessageOfItsOwn(t *testing.T) {
	p := newWeixinPlatform(t)
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
	if got := p.sent(); len(got) != 1 || got[0] != "The workspace is unreachable." {
		t.Fatalf("texts = %q, want the error once and not the draft", got)
	}
}

func TestReplySendsEachOutputAsAMessage(t *testing.T) {
	p := newWeixinPlatform(t)
	reply := p.open(t, time.Now())

	if err := reply.End(context.Background(), []channel.Message{{Text: "first"}, {Text: "second"}}, false); err != nil {
		t.Fatalf("End: %v", err)
	}
	if got := p.sent(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("texts = %q, want both outputs in order", got)
	}
}

// The context_token of an inbound message is kept 24 hours and every send needs
// one, so a reply to a message older than that fails with the window's code
// instead of the adapter's missing-token error.
func TestReplyWindowIsTheContextTokenLifetime(t *testing.T) {
	p := newWeixinPlatform(t)
	ctx := context.Background()

	inside := p.open(t, time.Now().Add(-23*time.Hour))
	if err := inside.End(ctx, []channel.Message{{Text: "in time"}}, false); err != nil {
		t.Fatalf("End inside the window: %v", err)
	}
	if got := p.sent(); len(got) != 1 {
		t.Fatalf("texts = %q, want the reply inside the window", got)
	}

	outside := p.open(t, time.Now().Add(-25*time.Hour))
	err := outside.End(ctx, []channel.Message{{Text: "too late"}}, false)
	if got := apperror.CodeOf(err); got != apperror.CodeChannelReplyWindowExpired {
		t.Fatalf("End after the window = %v (code %q), want %q", err, got, apperror.CodeChannelReplyWindowExpired)
	}
	if got := p.sent(); len(got) != 1 {
		t.Fatalf("texts = %q, want nothing sent after the window", got)
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
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: cfg.BotID, Channel: cfg.ChannelType, ReplyTarget: "user-1"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	return <-capture.sender
}
