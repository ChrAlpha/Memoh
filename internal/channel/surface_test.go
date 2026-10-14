package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/felinics/memoh/internal/channel/channeltest"
)

// previewAdapter is an adapter that shows replies through PreviewOpener. It
// records what the manager hands its preview.
type previewAdapter struct {
	channelType ChannelType
	caps        ChannelCapabilities
	policy      OutboundPolicy

	mu       sync.Mutex
	log      []string
	finals   []*PreparedStreamEvent
	updates  []PreparedStreamEvent
	messages []Message
	opens    []StreamOptions
	reacted  []string
}

func newPreviewAdapter(channelType ChannelType, limit int) *previewAdapter {
	return &previewAdapter{
		channelType: channelType,
		caps: ChannelCapabilities{
			Text: true, Markdown: true, Streaming: true, Attachments: true, Media: true, Reactions: true, Reply: true,
			Preview: PreviewEditInPlace,
		},
		policy: OutboundPolicy{TextChunkLimit: limit},
	}
}

func (a *previewAdapter) Type() ChannelType { return a.channelType }

func (a *previewAdapter) Descriptor() Descriptor {
	return Descriptor{Type: a.channelType, Capabilities: a.caps, OutboundPolicy: a.policy}
}

func (a *previewAdapter) Send(_ context.Context, _ ChannelConfig, msg PreparedOutboundMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, msg.LogicalMessage().Message)
	a.log = append(a.log, "send")
	return nil
}

func (a *previewAdapter) React(_ context.Context, _ ChannelConfig, target, messageID, emoji string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reacted = append(a.reacted, target+"/"+messageID+"/"+emoji)
	return nil
}

func (*previewAdapter) Unreact(context.Context, ChannelConfig, string, string, string) error {
	return nil
}

func (a *previewAdapter) OpenPreview(_ context.Context, _ ChannelConfig, _ string, opts StreamOptions) (PreparedPreview, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, opts)
	a.log = append(a.log, "open")
	return &adapterPreview{a: a}, nil
}

type adapterPreview struct{ a *previewAdapter }

func (p *adapterPreview) Update(_ context.Context, event PreparedStreamEvent) error {
	p.a.mu.Lock()
	defer p.a.mu.Unlock()
	p.a.updates = append(p.a.updates, event)
	p.a.log = append(p.a.log, "update:"+string(event.Type))
	return nil
}

func (p *adapterPreview) Finish(_ context.Context, final *PreparedStreamEvent) error {
	p.a.mu.Lock()
	defer p.a.mu.Unlock()
	p.a.finals = append(p.a.finals, final)
	if final == nil {
		p.a.log = append(p.a.log, "finish:none")
	} else {
		p.a.log = append(p.a.log, "finish:"+final.Final.Message.Message.Text)
	}
	return nil
}

func (p *adapterPreview) Abort(context.Context) error {
	p.a.mu.Lock()
	defer p.a.mu.Unlock()
	p.a.log = append(p.a.log, "abort")
	return nil
}

func (a *previewAdapter) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.log...)
}

func newPreviewSurface(t *testing.T, adapter *previewAdapter) *managerSurface {
	t.Helper()
	registry := NewRegistry()
	registry.MustRegister(adapter)
	manager := &Manager{registry: registry, attachmentStore: channeltest.NewMemoryAttachmentStore()}
	return manager.newSurface(ChannelConfig{BotID: "bot-1", ChannelType: adapter.channelType}, adapter.channelType)
}

func equalLog(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func finalOf(text string) *StreamEvent {
	return &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: Message{Text: text}}}
}

// A preview of an adapter that implements PreviewOpener gets the reply's
// events, and its final once; the completed status that ends a reply is not an
// update.
func TestSurfacePreviewDrivesAdapterPreview(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("preview-drive", 0)
	preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{SourceMessageID: "src-1"})
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	ctx := context.Background()
	if err := preview.Update(ctx, StreamEvent{Type: StreamEventStatus, Status: StreamStatusStarted}); err != nil {
		t.Fatal(err)
	}
	if err := preview.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "hello", Phase: StreamPhaseText}); err != nil {
		t.Fatal(err)
	}
	if err := preview.Finish(ctx, finalOf("hello world")); err != nil {
		t.Fatal(err)
	}
	if got := adapter.snapshot(); !equalLog(got, "open", "update:status", "update:delta", "finish:hello world") {
		t.Fatalf("adapter log = %v", got)
	}
	if len(adapter.opens) != 1 || adapter.opens[0].SourceMessageID != "src-1" || adapter.opens[0].Continuation {
		t.Fatalf("open options = %+v", adapter.opens)
	}
}

// A final longer than the platform takes is split by the manager: the preview
// ends with the first part and the rest are messages. The adapter's preview
// never sees text over the limit.
func TestSurfacePreviewSplitsLongFinal(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("preview-long", 40)
	preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{})
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	long := "first line of the reply\nsecond line of the reply\nthird line of the reply"
	if err := preview.Finish(context.Background(), finalOf(long)); err != nil {
		t.Fatal(err)
	}
	if len(adapter.finals) != 1 || adapter.finals[0].Final.Message.Message.Text != "first line of the reply" {
		t.Fatalf("finals = %+v", adapter.finals)
	}
	if got := adapter.snapshot(); !equalLog(got, "open", "finish:first line of the reply", "send", "send") {
		t.Fatalf("adapter log = %v", got)
	}
	rest := adapter.messages[0].Text + "\n" + adapter.messages[1].Text
	if rest != "second line of the reply\nthird line of the reply" {
		t.Fatalf("overflow = %q", rest)
	}
}

// Text that outgrows the platform while it streams moves to a new preview, so
// the adapter never holds more than one message of text.
func TestSurfacePreviewSplitsLongStream(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("preview-stream", 20)
	preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{})
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	ctx := context.Background()
	for _, delta := range []string{"first sentence. ", "second sentence. "} {
		if err := preview.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: delta, Phase: StreamPhaseText}); err != nil {
			t.Fatal(err)
		}
	}
	if len(adapter.opens) < 2 || !adapter.opens[1].Continuation {
		t.Fatalf("previews opened = %+v, want a continuation after the split", adapter.opens)
	}
	if got := adapter.snapshot(); len(got) < 3 || got[2] != "finish:" {
		t.Fatalf("adapter log = %v, want the first preview finished empty", got)
	}
}

// A preview that carries on after a split finishes without the text its
// earlier previews already showed.
func TestSurfaceContinuationFinalDoesNotRepeatText(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("preview-continuation", 0)
	preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{Continuation: true})
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	if err := preview.Finish(context.Background(), finalOf("all the text so far")); err != nil {
		t.Fatal(err)
	}
	if len(adapter.finals) != 1 || adapter.finals[0] == nil || adapter.finals[0].Final.Message.Message.Text != "" {
		t.Fatalf("finals = %+v, want an empty final", adapter.finals)
	}
	if len(adapter.messages) != 0 {
		t.Fatalf("messages = %v, want none", adapter.messages)
	}
}

// Abort keeps the text a preview shows, and aborts a preview that shows none.
func TestSurfacePreviewAbort(t *testing.T) {
	t.Parallel()

	t.Run("after text the preview ends with what it shows", func(t *testing.T) {
		t.Parallel()
		adapter := newPreviewAdapter("preview-abort-text", 0)
		preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = preview.Update(context.Background(), StreamEvent{Type: StreamEventDelta, Delta: "partial", Phase: StreamPhaseText})
		if err := preview.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := adapter.snapshot(); !equalLog(got, "open", "update:delta", "finish:") {
			t.Fatalf("adapter log = %v", got)
		}
	})
	t.Run("without text the preview is aborted", func(t *testing.T) {
		t.Parallel()
		adapter := newPreviewAdapter("preview-abort-empty", 0)
		preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = preview.Update(context.Background(), StreamEvent{Type: StreamEventDelta, Delta: "thinking", Phase: StreamPhaseReasoning})
		if err := preview.Abort(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := adapter.snapshot(); !equalLog(got, "open", "update:delta", "abort") {
			t.Fatalf("adapter log = %v", got)
		}
	})
}

// A reply with no message of its own ends its preview without a final.
func TestSurfacePreviewFinishWithoutFinal(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("preview-none", 0)
	preview, err := newPreviewSurface(t, adapter).OpenPreview(context.Background(), "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.Finish(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := adapter.snapshot(); !equalLog(got, "open", "finish:none") {
		t.Fatalf("adapter log = %v", got)
	}
}

// An adapter that declares a preview mode other than legacy has to implement
// PreviewOpener.
func TestSurfaceOpenPreviewNeedsOpener(t *testing.T) {
	t.Parallel()

	channelType := ChannelType("preview-missing-opener")
	registry := NewRegistry()
	registry.MustRegister(&streamValidationAdapter{
		channelType: channelType,
		dynamicCaps: &ChannelCapabilities{Text: true, Streaming: true, Preview: PreviewNativeStream},
	})
	manager := &Manager{registry: registry}
	_, err := manager.newSurface(ChannelConfig{BotID: "bot-1", ChannelType: channelType}, channelType).
		OpenPreview(context.Background(), "target-1", StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "does not open previews") {
		t.Fatalf("err = %v", err)
	}
}

// legacyAdapter is an adapter on the Push/Close session that PreviewLegacy
// names.
type legacyAdapter struct {
	channelType ChannelType
	stream      *closableStream
}

func (a *legacyAdapter) Type() ChannelType { return a.channelType }

func (a *legacyAdapter) Descriptor() Descriptor {
	return Descriptor{Type: a.channelType, Capabilities: ChannelCapabilities{Text: true, Markdown: true, Streaming: true}}
}

func (*legacyAdapter) Send(context.Context, ChannelConfig, PreparedOutboundMessage) error { return nil }

func (a *legacyAdapter) OpenStream(context.Context, ChannelConfig, string, StreamOptions) (PreparedOutboundStream, error) {
	a.stream = &closableStream{}
	return a.stream, nil
}

type closableStream struct {
	recordingStream
	closed bool
}

func (s *closableStream) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// A legacy adapter's preview is its Push/Close session: events are pushed, and
// the end of the reply is the final, the completed status and the close.
func TestSurfaceLegacyPreviewEndsTheSession(t *testing.T) {
	t.Parallel()

	channelType := ChannelType("preview-legacy")
	registry := NewRegistry()
	adapter := &legacyAdapter{channelType: channelType}
	registry.MustRegister(adapter)
	manager := &Manager{registry: registry, attachmentStore: channeltest.NewMemoryAttachmentStore()}
	preview, err := manager.newSurface(ChannelConfig{BotID: "bot-1", ChannelType: channelType}, channelType).
		OpenPreview(context.Background(), "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = preview.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "hello", Phase: StreamPhaseText})
	if err := preview.Finish(ctx, finalOf("hello")); err != nil {
		t.Fatal(err)
	}
	var types []StreamEventType
	for _, event := range adapter.stream.Events() {
		types = append(types, event.Type)
	}
	want := []StreamEventType{StreamEventDelta, StreamEventFinal, StreamEventStatus}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
	if !adapter.stream.closed {
		t.Fatal("session was not closed")
	}
}

// Attach sends the attachment as a message of its own through the one send
// path, quoting the reply it is given.
func TestSurfaceAttachSendsOneAttachment(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter(ChannelTypeTelegram, 0)
	surface := newPreviewSurface(t, adapter)
	att := Attachment{Type: AttachmentImage, URL: "https://example.test/a.png", Name: "a.png"}
	reply := &ReplyRef{Target: "target-1", MessageID: "m-1"}
	if err := surface.Attach(context.Background(), "target-1", att, reply); err != nil {
		t.Fatal(err)
	}
	if err := surface.Attach(context.Background(), "target-1", att, nil); err != nil {
		t.Fatal(err)
	}
	if len(adapter.messages) != 2 || len(adapter.messages[0].Attachments) != 1 || adapter.messages[0].Attachments[0].URL != att.URL {
		t.Fatalf("messages = %+v", adapter.messages)
	}
	if adapter.messages[0].Text != "" {
		t.Fatalf("attachment message carries text %q", adapter.messages[0].Text)
	}
	if adapter.messages[0].Reply == nil || adapter.messages[0].Reply.MessageID != "m-1" {
		t.Fatalf("attachment quotes %+v, want the reply it was given", adapter.messages[0].Reply)
	}
	if adapter.messages[1].Reply != nil {
		t.Fatalf("attachment without a reply quotes %+v", adapter.messages[1].Reply)
	}
}

func TestSurfaceReact(t *testing.T) {
	t.Parallel()

	adapter := newPreviewAdapter("preview-react", 0)
	surface := newPreviewSurface(t, adapter)
	if err := surface.React(context.Background(), ReactRequest{Target: "target-1", MessageID: "m-1", Emoji: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(adapter.reacted) != 1 || adapter.reacted[0] != "target-1/m-1/ok" {
		t.Fatalf("reactions = %v", adapter.reacted)
	}
	for name, req := range map[string]ReactRequest{
		"no target":  {MessageID: "m-1", Emoji: "ok"},
		"no message": {Target: "target-1", Emoji: "ok"},
		"no emoji":   {Target: "target-1", MessageID: "m-1"},
	} {
		if err := surface.React(context.Background(), req); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	channelType := ChannelType("preview-no-reactor")
	registry := NewRegistry()
	registry.MustRegister(&streamValidationAdapter{channelType: channelType})
	manager := &Manager{registry: registry}
	err := manager.newSurface(ChannelConfig{BotID: "bot-1", ChannelType: channelType}, channelType).
		React(context.Background(), ReactRequest{Target: "t", MessageID: "m", Emoji: "ok"})
	if err == nil || !strings.Contains(err.Error(), "does not support reactions") {
		t.Fatalf("err = %v", err)
	}
}

// Capabilities are refined for the target.
func TestSurfaceCapabilitiesAreRefinedPerTarget(t *testing.T) {
	t.Parallel()

	channelType := ChannelType("surface-caps")
	registry := NewRegistry()
	registry.MustRegister(&targetResolvingAdapter{channelType: channelType, dynamicCaps: &ChannelCapabilities{Text: true, Preview: PreviewDraft}})
	manager := &Manager{registry: registry}
	got := manager.newSurface(ChannelConfig{BotID: "bot-1", ChannelType: channelType}, channelType).Capabilities("target-1")
	if got.Preview != PreviewDraft || !got.Text {
		t.Fatalf("capabilities = %+v", got)
	}
}

// A message sent by Manager.Send and the same message sent through a reply's
// surface reach the adapter identically: size, format and retries are decided
// once.
func TestManagerSendAndSurfaceSendDeliverAlike(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a line of the reply that goes on and on\n", 6)
	deliver := func(through func(*Manager, ChannelConfig, ChannelType) error) []Message {
		adapter := newPreviewAdapter(ChannelTypeTelegram, 60)
		registry := NewRegistry()
		registry.MustRegister(adapter)
		cfg := ChannelConfig{BotID: "bot-1", ChannelType: ChannelTypeTelegram}
		manager := &Manager{registry: registry, service: sendStore{cfg: cfg}, attachmentStore: channeltest.NewMemoryAttachmentStore()}
		if err := through(manager, cfg, ChannelTypeTelegram); err != nil {
			t.Fatal(err)
		}
		return adapter.messages
	}
	viaManager := deliver(func(m *Manager, _ ChannelConfig, typ ChannelType) error {
		return m.Send(context.Background(), "bot-1", typ, SendRequest{Target: "target-1", Message: Message{Text: long}})
	})
	viaSurface := deliver(func(m *Manager, cfg ChannelConfig, typ ChannelType) error {
		return m.newSurface(cfg, typ).Send(context.Background(), OutboundMessage{Target: "target-1", Message: Message{Text: long}})
	})
	if len(viaManager) < 2 {
		t.Fatalf("message was not split: %d parts", len(viaManager))
	}
	if len(viaManager) != len(viaSurface) {
		t.Fatalf("manager sent %d parts, surface %d", len(viaManager), len(viaSurface))
	}
	for i := range viaManager {
		if viaManager[i].Text != viaSurface[i].Text || viaManager[i].Format != viaSurface[i].Format {
			t.Fatalf("part %d differs: %+v vs %+v", i, viaManager[i], viaSurface[i])
		}
	}
}

type sendStore struct{ cfg ChannelConfig }

func (s sendStore) ListConfigsByType(context.Context, ChannelType) ([]ChannelConfig, error) {
	return []ChannelConfig{s.cfg}, nil
}

func (s sendStore) ResolveEffectiveConfig(context.Context, string, ChannelType) (ChannelConfig, error) {
	return s.cfg, nil
}

func (sendStore) GetChannelIdentityConfig(context.Context, string, ChannelType) (ChannelIdentityBinding, error) {
	return ChannelIdentityBinding{}, errors.New("not used")
}
