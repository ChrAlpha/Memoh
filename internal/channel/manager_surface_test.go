package channel

import (
	"context"
	"reflect"
	"testing"

	"github.com/felinics/memoh/internal/channel/channeltest"
)

// The surface a manager hands out for a bot's replies outside an inbound
// message is the one an inbound message gets: the channel's own previews and
// capabilities, over the bot's effective configuration. Nothing is taken off
// it, so an approval or a question the channel renders itself is still
// rendered by it.
func TestManagerSurfaceIsTheSurfaceOfAnInboundReply(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("surface-queued", 0)
	adapter.caps.NativeApproval = true
	adapter.caps.NativeUserInput = true
	registry := NewRegistry()
	registry.MustRegister(adapter)
	store := &fakeConfigStore{effectiveConfig: ChannelConfig{BotID: "bot-1", ChannelType: adapter.channelType}}
	manager := NewManager(nil, registry, store, nil)
	manager.attachmentStore = channeltest.NewMemoryAttachmentStore()

	surface, err := manager.Surface(context.Background(), "bot-1", adapter.channelType)
	if err != nil {
		t.Fatalf("Surface() error = %v", err)
	}
	caps := surface.Capabilities("target-1")
	want, _ := registry.GetOutboundCapabilities(adapter.channelType, ChannelConfig{BotID: "bot-1", ChannelType: adapter.channelType}, "target-1")
	if !reflect.DeepEqual(caps, want) {
		t.Fatalf("capabilities = %+v, want the channel's own %+v", caps, want)
	}
	if !caps.NativeApproval || !caps.NativeUserInput || caps.Preview == PreviewNone {
		t.Fatalf("capabilities = %+v, want the channel's own preview, approvals and questions", caps)
	}
	preview, err := surface.OpenPreview(context.Background(), "target-1", StreamOptions{SourceMessageID: "src-1"})
	if err != nil {
		t.Fatalf("OpenPreview() error = %v", err)
	}
	if err := preview.Finish(context.Background(), finalOf("answer")); err != nil {
		t.Fatalf("Finish() error = %v", err)
	}
	if got := adapter.snapshot(); !equalLog(got, "open", "finish:answer") {
		t.Fatalf("adapter log = %v", got)
	}
	if err := surface.React(context.Background(), ReactRequest{Target: "target-1", MessageID: "src-1", Emoji: "x", Remove: true}); err != nil {
		t.Fatalf("React() error = %v", err)
	}
}

func TestManagerSurfaceRefusesWhatHasNoSenderOrNoConfig(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	manager := NewManager(nil, registry, &fakeConfigStore{}, nil)
	if _, err := manager.Surface(context.Background(), "bot-1", "no-such-channel"); err == nil {
		t.Fatal("a channel with no sender got a surface")
	}
	if _, err := (&Manager{registry: registry}).Surface(context.Background(), "bot-1", "no-such-channel"); err == nil {
		t.Fatal("a manager with no store got a surface")
	}
}
