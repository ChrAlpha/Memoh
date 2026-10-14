package discord

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

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
// manager, over the real adapter, with only the Discord API replaced.
func openReply(t *testing.T) (*delivery.Reply, *discordAPI) {
	t.Helper()
	api := &discordAPI{}
	session, err := discordgo.New("Bot tok")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	session.Client = &http.Client{Transport: api}
	adapter := NewDiscordAdapter(nil)
	adapter.sessions[discordSessionCacheKey("cfg-1", "tok")] = session

	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: Type, Credentials: map[string]any{"botToken": "tok"}}
	processor := &seamProcessor{sender: make(chan channel.StreamReplySender, 1), release: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, seamStore{cfg: cfg}, processor)
	t.Cleanup(func() {
		close(processor.release)
		_ = manager.Shutdown(context.Background())
	})
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: "bot-1", Channel: Type, ReplyTarget: "ch-1"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         <-processor.sender,
		Target:          "ch-1",
		Reply:           &channel.ReplyRef{Target: "ch-1", MessageID: "src-1"},
		SourceMessageID: "src-1",
	})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	t.Cleanup(func() { _ = reply.Close(context.Background()) })
	if err := reply.Start(context.Background()); err != nil {
		t.Fatalf("start reply: %v", err)
	}
	return reply, api
}

func delta(text string) channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}
}

// A reply that the manager drives shows as one message: the placeholder, then
// the final text in its place.
func TestDiscordReplyThroughManagerEditsOneMessage(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	ctx := context.Background()
	for _, ev := range []channel.StreamEvent{delta("hel"), delta("lo")} {
		if err := reply.Show(ctx, ev); err != nil {
			t.Fatalf("show: %v", err)
		}
	}
	if err := reply.End(ctx, []channel.Message{{Text: "hello"}}, false); err != nil {
		t.Fatalf("end: %v", err)
	}

	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "PATCH /channels/ch-1/messages/msg-1") {
		t.Fatalf("calls = %v", got)
	}
	if got := api.callAt(t, 1).content(); got != "hello" {
		t.Fatalf("final text = %q", got)
	}
}

// A run that says nothing leaves no placeholder behind.
func TestDiscordReplyThroughManagerRemovesThePlaceholderOfASilentRun(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	if err := reply.End(context.Background(), nil, false); err != nil {
		t.Fatalf("end: %v", err)
	}
	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "DELETE /channels/ch-1/messages/msg-1") {
		t.Fatalf("calls = %v", got)
	}
}

// An error before any text takes the place of the placeholder, and ending the
// reply afterwards leaves it.
func TestDiscordReplyThroughManagerShowsAnErrorInPlaceOfThePlaceholder(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	ctx := context.Background()
	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if err := reply.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "PATCH /channels/ch-1/messages/msg-1") {
		t.Fatalf("calls = %v", got)
	}
	if got := api.callAt(t, 1).content(); got != "The workspace is unreachable." {
		t.Fatalf("error text = %q", got)
	}
}

// Discord renders no approval request itself, so the request is a message of
// its own that tells the user how to answer.
func TestDiscordReplyThroughManagerSendsTheApprovalPromptAsText(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	err := reply.Show(context.Background(), channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "exec", CallID: "call-1", ApprovalID: "approval-1", ShortID: 7,
		Input: map[string]any{"command": "pwd"},
	}})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if got := api.summary(); !equalStrings(got, "POST /channels/ch-1/messages", "POST /channels/ch-1/messages") {
		t.Fatalf("calls = %v", got)
	}
	if got := api.callAt(t, 1).content(); !strings.Contains(got, "/approve 7") {
		t.Fatalf("prompt = %q", got)
	}
}
