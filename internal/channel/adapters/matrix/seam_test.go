package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
// manager, over the real adapter, with only the homeserver replaced.
func openReply(t *testing.T) (*delivery.Reply, *matrixHomeserver) {
	t.Helper()
	server := &matrixHomeserver{}
	adapter := NewMatrixAdapter(nil)
	adapter.httpClient = &http.Client{Transport: server}

	registry := channel.NewRegistry()
	registry.MustRegister(adapter)
	cfg := channel.ChannelConfig{ID: "cfg-1", BotID: "bot-1", ChannelType: Type, Credentials: map[string]any{
		"homeserverUrl": "https://matrix.example.com",
		"accessToken":   "tok",
		"userId":        "@bot:example.com",
	}}
	processor := &seamProcessor{sender: make(chan channel.StreamReplySender, 1), release: make(chan struct{})}
	manager := channel.NewManager(slog.New(slog.DiscardHandler), registry, seamStore{cfg: cfg}, processor)
	t.Cleanup(func() {
		close(processor.release)
		_ = manager.Shutdown(context.Background())
	})
	if err := manager.HandleInbound(context.Background(), cfg, channel.InboundMessage{BotID: "bot-1", Channel: Type, ReplyTarget: "!room:example.com"}); err != nil {
		t.Fatalf("enqueue inbound: %v", err)
	}
	reply, err := delivery.Open(context.Background(), delivery.Config{
		Surface:         <-processor.sender,
		Target:          "!room:example.com",
		Reply:           &channel.ReplyRef{Target: "!room:example.com", MessageID: "$src"},
		SourceMessageID: "$src",
	})
	if err != nil {
		t.Fatalf("open reply: %v", err)
	}
	t.Cleanup(func() { _ = reply.Close(context.Background()) })
	if err := reply.Start(context.Background()); err != nil {
		t.Fatalf("start reply: %v", err)
	}
	return reply, server
}

// The text of a reply is sent once, quoting the message it answers, and the
// final needs no edit when nothing changed it.
func TestMatrixReplyThroughManagerSendsTheFinalText(t *testing.T) {
	t.Parallel()

	reply, server := openReply(t)
	ctx := context.Background()
	for _, text := range []string{"hel", "lo"} {
		if err := reply.Show(ctx, textDelta(text)); err != nil {
			t.Fatalf("show: %v", err)
		}
	}
	if err := reply.End(ctx, []channel.Message{{Text: "hello"}}, false); err != nil {
		t.Fatalf("end: %v", err)
	}

	if got := len(server.sent()); got != 1 {
		t.Fatalf("sent %d events, want one", got)
	}
	content := server.content(t, 0)
	relates, _ := content["m.relates_to"].(map[string]any)
	if reply, _ := relates["m.in_reply_to"].(map[string]any); content["body"] != "hello" || reply["event_id"] != "$src" {
		t.Fatalf("event = %v", content)
	}
}

// A run that says nothing sends nothing: Matrix shows no placeholder.
func TestMatrixReplyThroughManagerSendsNothingForASilentRun(t *testing.T) {
	t.Parallel()

	reply, server := openReply(t)
	if err := reply.End(context.Background(), nil, false); err != nil {
		t.Fatalf("end: %v", err)
	}
	if got := server.sent(); len(got) != 0 {
		t.Fatalf("sent = %q", got)
	}
}

func TestMatrixReplyThroughManagerSendsAnErrorAsText(t *testing.T) {
	t.Parallel()

	reply, server := openReply(t)
	ctx := context.Background()
	if err := reply.Show(ctx, channel.StreamEvent{Type: channel.StreamEventError, Error: "The workspace is unreachable.", ErrorCode: "workspace.unreachable"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	if err := reply.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := len(server.sent()); got != 1 || server.content(t, 0)["body"] != "The workspace is unreachable." {
		t.Fatalf("sent = %q", server.sent())
	}
}

// Matrix renders no approval request itself, so the request is a message that
// tells the user how to answer.
func TestMatrixReplyThroughManagerSendsTheApprovalPromptAsText(t *testing.T) {
	t.Parallel()

	reply, server := openReply(t)
	err := reply.Show(context.Background(), channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "exec", CallID: "call-1", ApprovalID: "approval-1", ShortID: 7,
		Input: map[string]any{"command": "pwd"},
	}})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if got := len(server.sent()); got != 1 || !strings.Contains(fmt.Sprint(server.content(t, 0)["body"]), "/approve 7") {
		t.Fatalf("sent = %q", server.sent())
	}
}
