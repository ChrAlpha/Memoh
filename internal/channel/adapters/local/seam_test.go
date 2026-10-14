package local

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/delivery"
)

type seamStore struct{ cfg channel.ChannelConfig }

func (s seamStore) ListConfigsByType(context.Context, channel.ChannelType) ([]channel.ChannelConfig, error) {
	return []channel.ChannelConfig{s.cfg}, nil
}

func (s seamStore) ResolveEffectiveConfig(context.Context, string, channel.ChannelType) (channel.ChannelConfig, error) {
	return s.cfg, nil
}

func (seamStore) GetChannelIdentityConfig(context.Context, string, channel.ChannelType) (channel.ChannelIdentityBinding, error) {
	return channel.ChannelIdentityBinding{}, errors.New("not used")
}

// seamProcessor hands the reply sender a real manager makes for an inbound
// message to the test, and holds it until release is closed.
type seamProcessor struct {
	sender  chan channel.StreamReplySender
	release chan struct{}
}

func (p *seamProcessor) HandleInbound(_ context.Context, _ channel.ChannelConfig, _ channel.InboundMessage, sender channel.StreamReplySender) error {
	p.sender <- sender
	<-p.release
	return nil
}

// openReply opens a reply the way an inbound message does: through a real
// manager over the real Web adapter. seen is what a subscriber of the route
// receives.
func openReply(t *testing.T) (*delivery.Reply, func() []channel.StreamEvent) {
	t.Helper()
	hub := NewRouteHub()
	seen := subscribe(t, hub, "bot-1")
	registry := channel.NewRegistry()
	registry.MustRegister(NewWebAdapter(hub))
	cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: WebType}
	processor := &seamProcessor{sender: make(chan channel.StreamReplySender, 1), release: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, seamStore{cfg: cfg}, processor)
	t.Cleanup(func() {
		close(processor.release)
		_ = manager.Shutdown(context.Background())
	})
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: "bot-1", Channel: WebType, ReplyTarget: "bot-1"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	reply, err := delivery.Open(context.Background(), delivery.Config{Surface: <-processor.sender, Target: "bot-1"})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	t.Cleanup(func() { _ = reply.Close(context.Background()) })
	if err := reply.Start(context.Background()); err != nil {
		t.Fatalf("start reply: %v", err)
	}
	return reply, seen
}

// Subscribers of the route see the reply as the stream the run produces, and
// the completed status after the final.
func TestWebReplyThroughManagerStreamsEveryEvent(t *testing.T) {
	t.Parallel()

	reply, seen := openReply(t)
	ctx := context.Background()
	for _, text := range []string{"hel", "lo"} {
		if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}); err != nil {
			t.Fatalf("show: %v", err)
		}
	}
	if err := reply.End(ctx, []channel.Message{{Text: "hello"}}, false); err != nil {
		t.Fatalf("end: %v", err)
	}

	got := seen()
	if want := "status/started,delta/,delta/,final/,status/completed"; strings.Join(types(got), ",") != want {
		t.Fatalf("subscriber saw %v, want %s", types(got), want)
	}
	if got[3].Final.Message.Text != "hello" {
		t.Fatalf("final = %+v", got[3].Final)
	}
}

// A run that says nothing still closes the reply for subscribers.
func TestWebReplyThroughManagerClosesASilentRun(t *testing.T) {
	t.Parallel()

	reply, seen := openReply(t)
	if err := reply.End(context.Background(), nil, false); err != nil {
		t.Fatalf("end: %v", err)
	}
	if got, want := strings.Join(types(seen()), ","), "status/started,status/completed"; got != want {
		t.Fatalf("subscriber saw %s, want %s", got, want)
	}
}

// The Web UI renders approval requests from the stream, so the request reaches
// subscribers as the event it is.
func TestWebReplyThroughManagerForwardsTheApprovalRequest(t *testing.T) {
	t.Parallel()

	reply, seen := openReply(t)
	err := reply.Show(context.Background(), channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "exec", CallID: "call-1", ApprovalID: "approval-1", ShortID: 7,
		Input: map[string]any{"command": "pwd"},
	}})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	got := seen()
	if len(got) != 2 || got[1].Type != channel.StreamEventToolCallStart || got[1].ToolCall == nil || got[1].ToolCall.ApprovalID != "approval-1" {
		t.Fatalf("subscriber saw %+v", got)
	}
}
