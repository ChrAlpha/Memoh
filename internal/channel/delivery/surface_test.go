package delivery

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/channel"
)

// transport stands in for an adapter's wire. It keeps the descriptor it is
// given, so the manager makes the routing decisions it makes in production,
// and records what would reach the platform.
type transport struct {
	desc channel.Descriptor

	mu       sync.Mutex
	log      []string
	messages []channel.Message
	// finals holds the final each preview ended with.
	finals []*channel.Message
	opens  int
}

func (t *transport) Type() channel.ChannelType { return t.desc.Type }

func (t *transport) Descriptor() channel.Descriptor { return t.desc }

func (t *transport) record(entry string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.log = append(t.log, entry)
}

func (t *transport) Send(_ context.Context, _ channel.ChannelConfig, msg channel.PreparedOutboundMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messages = append(t.messages, msg.LogicalMessage().Message)
	t.log = append(t.log, "send")
	return nil
}

func (t *transport) OpenPreview(context.Context, channel.ChannelConfig, string, channel.StreamOptions) (channel.PreparedPreview, error) {
	t.mu.Lock()
	t.opens++
	t.mu.Unlock()
	return &transportPreview{t: t}, nil
}

func (t *transport) React(_ context.Context, _ channel.ChannelConfig, _, messageID, emoji string) error {
	t.record("react:" + messageID + ":" + emoji)
	return nil
}

func (*transport) Unreact(context.Context, channel.ChannelConfig, string, string, string) error {
	return nil
}

func (t *transport) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.log...)
}

type transportPreview struct{ t *transport }

func (p *transportPreview) Update(_ context.Context, event channel.PreparedStreamEvent) error {
	p.t.record("update:" + string(event.Type))
	return nil
}

func (p *transportPreview) Finish(_ context.Context, final *channel.PreparedStreamEvent) error {
	if final == nil {
		p.t.record("finish:none")
		return nil
	}
	msg := final.Final.Message.LogicalMessage()
	p.t.mu.Lock()
	p.t.finals = append(p.t.finals, &msg)
	p.t.mu.Unlock()
	p.t.record("finish")
	return nil
}

func (p *transportPreview) Abort(context.Context) error {
	p.t.record("abort")
	return nil
}

type storeStub struct{ cfg channel.ChannelConfig }

func (s storeStub) ListConfigsByType(context.Context, channel.ChannelType) ([]channel.ChannelConfig, error) {
	return []channel.ChannelConfig{s.cfg}, nil
}

func (s storeStub) ResolveEffectiveConfig(context.Context, string, channel.ChannelType) (channel.ChannelConfig, error) {
	return s.cfg, nil
}

func (storeStub) GetChannelIdentityConfig(context.Context, string, channel.ChannelType) (channel.ChannelIdentityBinding, error) {
	return channel.ChannelIdentityBinding{}, errors.New("not used")
}

// captureProcessor hands the reply sender the manager makes for an inbound
// message to the test, which holds it until release is closed.
type captureProcessor struct {
	sender  chan channel.StreamReplySender
	release chan struct{}
}

func (c *captureProcessor) HandleInbound(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, sender channel.StreamReplySender) error {
	c.sender <- sender
	<-c.release
	return nil
}

// managerSender returns the reply sender a real manager makes for adapter.
func managerSender(t *testing.T, adapter channel.Adapter) channel.StreamReplySender {
	t.Helper()
	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	cfg := channel.ChannelConfig{BotID: "bot-1", ChannelType: adapter.Type()}
	capture := &captureProcessor{sender: make(chan channel.StreamReplySender, 1), release: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, storeStub{cfg: cfg}, capture)
	t.Cleanup(func() {
		close(capture.release)
		_ = manager.Shutdown(context.Background())
	})
	msg := channel.InboundMessage{BotID: "bot-1", Channel: adapter.Type(), ReplyTarget: "target-1"}
	if err := manager.HandleInbound(context.Background(), cfg, msg); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	return <-capture.sender
}

func hasEntry(log []string, prefix string) bool {
	for _, entry := range log {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
