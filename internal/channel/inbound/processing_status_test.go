package inbound

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/identities"
	"github.com/felinics/memoh/internal/channel/route"
)

// refreshingStatusAdapter is a channel whose processing indicator expires,
// like Telegram's typing action. It is safe for the concurrent refresh loop.
type refreshingStatusAdapter struct {
	mu        sync.Mutex
	events    []string
	infos     []channel.ProcessingStatusInfo
	refreshes int
	interval  time.Duration
}

func (*refreshingStatusAdapter) Type() channel.ChannelType { return channel.ChannelType("feishu") }

func (*refreshingStatusAdapter) Descriptor() channel.Descriptor {
	return channel.Descriptor{
		Type:         channel.ChannelType("feishu"),
		Capabilities: channel.ChannelCapabilities{Text: true, Reply: true},
	}
}

func (a *refreshingStatusAdapter) record(event string, info channel.ProcessingStatusInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	a.infos = append(a.infos, info)
	if event == "refresh" {
		a.refreshes++
	}
}

func (a *refreshingStatusAdapter) refreshCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshes
}

func (a *refreshingStatusAdapter) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

func (a *refreshingStatusAdapter) ProcessingStarted(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, info channel.ProcessingStatusInfo) (channel.ProcessingStatusHandle, error) {
	a.record("started", info)
	return channel.ProcessingStatusHandle{Token: "tok"}, nil
}

func (a *refreshingStatusAdapter) ProcessingCompleted(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, info channel.ProcessingStatusInfo, _ channel.ProcessingStatusHandle) error {
	a.record("completed", info)
	return nil
}

func (a *refreshingStatusAdapter) ProcessingFailed(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, info channel.ProcessingStatusInfo, _ channel.ProcessingStatusHandle, _ error) error {
	a.record("failed", info)
	return nil
}

func (a *refreshingStatusAdapter) ProcessingRefreshInterval() time.Duration { return a.interval }

func (a *refreshingStatusAdapter) ProcessingRefresh(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, info channel.ProcessingStatusInfo, _ channel.ProcessingStatusHandle) error {
	a.record("refresh", info)
	return nil
}

// finalObservingSender records how many refreshes had happened when the
// final reply went out, whether as a message or as a preview's final.
type finalObservingSender struct {
	fakeReplySender
	adapter          *refreshingStatusAdapter
	refreshesAtFinal int
}

func (s *finalObservingSender) Send(ctx context.Context, msg channel.OutboundMessage) error {
	s.refreshesAtFinal = s.adapter.refreshCount()
	// Delivering takes a while; a refresh still running would land in it.
	time.Sleep(20 * time.Millisecond)
	return s.fakeReplySender.Send(ctx, msg)
}

func (s *finalObservingSender) OpenPreview(ctx context.Context, target string, opts channel.StreamOptions) (channel.Preview, error) {
	return openStreamPreview(ctx, s, target, opts)
}

func (s *finalObservingSender) OpenStream(ctx context.Context, target string, opts channel.StreamOptions) (channel.OutboundStream, error) {
	stream, err := s.fakeReplySender.OpenStream(ctx, target, opts)
	if err != nil {
		return nil, err
	}
	return &finalObservingStream{OutboundStream: stream, sender: s}, nil
}

type finalObservingStream struct {
	channel.OutboundStream
	sender *finalObservingSender
}

func (s *finalObservingStream) Push(ctx context.Context, event channel.StreamEvent) error {
	if event.Type == channel.StreamEventFinal && event.Final != nil && !event.Final.Message.IsEmpty() {
		s.sender.refreshesAtFinal = s.sender.adapter.refreshCount()
		// Delivering takes a while; a refresh still running would land in it.
		time.Sleep(20 * time.Millisecond)
	}
	return s.OutboundStream.Push(ctx, event)
}

func TestHandleInboundKeepsProcessingIndicatorAliveUntilFinalReply(t *testing.T) {
	adapter := &refreshingStatusAdapter{interval: 2 * time.Millisecond}
	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	chatSvc := &fakeChatService{resolveResult: route.ResolveConversationResult{BotID: "bot-1", RouteID: "route-1"}}
	gateway := &fakeChatGateway{
		resp: fakeChatResponse{Messages: []turn.ModelMessage{
			{Role: "assistant", Content: turn.NewTextContent("AI reply")},
		}},
		// A slow model: the run is under way, and its events arrive late.
		whileRunning: func() {
			deadline := time.Now().Add(2 * time.Second)
			for adapter.refreshCount() < 3 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		},
	}
	processor := NewChannelInboundProcessor(slog.Default(), registry, chatSvc, chatSvc, gateway,
		&fakeChannelIdentityService{channelIdentity: identities.ChannelIdentity{ID: "ci-1"}}, &fakePolicyService{}, "", 0)
	sender := &finalObservingSender{adapter: adapter}
	cfg := channel.ChannelConfig{TeamID: "team-test", ID: "cfg-1", BotID: "bot-1", ChannelType: channel.ChannelType("feishu")}
	msg := channel.InboundMessage{
		BotID:        "bot-1",
		Channel:      channel.ChannelType("feishu"),
		Message:      channel.Message{ID: "om_1", Text: "hello"},
		ReplyTarget:  "chat_id:oc_1",
		Sender:       channel.Identity{SubjectID: "ext-1"},
		Conversation: channel.Conversation{ID: "oc_1", Type: channel.ConversationTypePrivate},
	}

	if err := processor.HandleInbound(context.Background(), cfg, msg, sender); err != nil {
		t.Fatalf("HandleInbound: %v", err)
	}
	if got := adapter.refreshCount(); got < 3 {
		t.Fatalf("refreshes during the turn = %d, want at least 3", got)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("replies = %d, want 1", len(sender.sent))
	}
	finalCount := adapter.refreshCount()
	if sender.refreshesAtFinal != finalCount {
		t.Fatalf("refreshes at final reply = %d, after the turn = %d; want none after the reply", sender.refreshesAtFinal, finalCount)
	}
	time.Sleep(20 * time.Millisecond)
	if got := adapter.refreshCount(); got != finalCount {
		t.Fatalf("refresh fired after HandleInbound returned: %d -> %d", finalCount, got)
	}
	events := adapter.snapshot()
	if events[0] != "started" || events[len(events)-1] != "completed" {
		t.Fatalf("events = %v, want started ... completed", events)
	}
}
