package slack

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/slack-go/slack"

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
// manager, over the real adapter, with only the Slack API replaced.
func openReply(t *testing.T) (*delivery.Reply, *slackAPI) {
	t.Helper()
	api := &slackAPI{}
	adapter := NewSlackAdapter(nil)
	adapter.apiFactory = func(cfg Config, options ...slack.Option) *slack.Client {
		options = append([]slack.Option{
			slack.OptionAPIURL("https://slack.test/api/"),
			slack.OptionHTTPClient(&http.Client{Transport: api}),
		}, options...)
		return slack.New(cfg.BotToken, options...)
	}

	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: Type, Credentials: map[string]any{
		"botToken": testShortBotToken,
		"appToken": testAppToken,
	}}
	processor := &seamProcessor{sender: make(chan channel.StreamReplySender, 1), release: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, seamStore{cfg: cfg}, processor)
	t.Cleanup(func() {
		close(processor.release)
		_ = manager.Shutdown(context.Background())
	})
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: "bot-1", Channel: Type, ReplyTarget: "C123"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         <-processor.sender,
		Target:          "C123",
		Reply:           &channel.ReplyRef{Target: "C123", MessageID: "1710000000.000050"},
		SourceMessageID: "1710000000.000050",
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

// A reply that the manager drives shows as one message in the thread of the
// message it answers: the placeholder, then the final text in its place.
func TestSlackReplyThroughManagerUpdatesOneMessage(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	ctx := context.Background()
	for _, text := range []string{"hel", "lo"} {
		if err := reply.Show(ctx, textDeltaEvent(text)); err != nil {
			t.Fatalf("show: %v", err)
		}
	}
	if err := reply.End(ctx, []channel.Message{{Text: "hello"}}, false); err != nil {
		t.Fatalf("end: %v", err)
	}

	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.update") {
		t.Fatalf("calls = %v", got)
	}
	calls := api.snapshot()
	if calls[0].Form.Get("thread_ts") != "1710000000.000050" || calls[1].text() != "hello" {
		t.Fatalf("calls = %v, %v", calls[0].Form, calls[1].Form)
	}
}

// A run that says nothing leaves no placeholder behind.
func TestSlackReplyThroughManagerRemovesThePlaceholderOfASilentRun(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	if err := reply.End(context.Background(), nil, false); err != nil {
		t.Fatalf("end: %v", err)
	}
	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.delete") {
		t.Fatalf("calls = %v", got)
	}
}

// An error before any text takes the place of the placeholder, and ending the
// reply afterwards leaves it.
func TestSlackReplyThroughManagerShowsAnErrorInPlaceOfThePlaceholder(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	ctx := context.Background()
	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if err := reply.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.update") {
		t.Fatalf("calls = %v", got)
	}
	if got := api.snapshot()[1].text(); got != "The workspace is unreachable." {
		t.Fatalf("error text = %q", got)
	}
}

// Slack renders no approval request itself, so the request is a message of its
// own that tells the user how to answer.
func TestSlackReplyThroughManagerSendsTheApprovalPromptAsText(t *testing.T) {
	t.Parallel()

	reply, api := openReply(t)
	err := reply.Show(context.Background(), channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "exec", CallID: "call-1", ApprovalID: "approval-1", ShortID: 7,
		Input: map[string]any{"command": "pwd"},
	}})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if got := api.summary(); !equalStrings(got, "chat.postMessage", "chat.postMessage") {
		t.Fatalf("calls = %v", got)
	}
	if got := api.snapshot()[1].text(); !strings.Contains(got, "/approve 7") {
		t.Fatalf("prompt = %q", got)
	}
}

func textDeltaEvent(text string) channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventDelta, Delta: text, Phase: channel.StreamPhaseText}
}
