package channel

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/channel/channeltest"
	"github.com/felinics/memoh/internal/markdownmedia"
	"github.com/felinics/memoh/internal/media"
)

func TestMarkdownMediaAcrossChannelPreparation(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a2ioAAAAASUVORK5CYII=")
	for _, platform := range []string{"discord", "telegram", "feishu", "dingtalk", "qq", "slack", "matrix", "misskey", "line", "wecom", "weixin", "wechatoa"} {
		t.Run(platform, func(t *testing.T) {
			ctx := context.Background()
			store := channeltest.NewMemoryAttachmentStore()
			store.SeedContainerFile("bot-1", "/data/截图.png", png, "image/png", "截图.png")
			store.SeedContainerFile("bot-1", "/data/report.txt", []byte("report bytes"), "text/plain", "report.txt")
			cfg := ChannelConfig{BotID: "bot-1", ChannelType: ChannelType(platform)}
			msg := resolveMarkdownMessage(ctx, store, cfg, Message{Text: "before\n\n![截图](/data/截图.png)\n\nmiddle\n\n[report](/data/report.txt)\n\nafter", Format: MessageFormatMarkdown})
			parts, err := buildOutboundMessages(OutboundMessage{Message: msg}, OutboundPolicy{})
			if err != nil || len(parts) != 5 {
				t.Fatalf("parts=%+v err=%v", parts, err)
			}
			for i, part := range parts {
				prepared, err := PrepareOutboundMessage(ctx, store, cfg, part)
				if err != nil {
					t.Fatal(err)
				}
				if i != 1 && i != 3 {
					continue
				}
				if len(prepared.Message.Attachments) != 1 {
					t.Fatalf("attachments=%+v", prepared)
				}
				reader, err := prepared.Message.Attachments[0].Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				data, _ := io.ReadAll(reader)
				_ = reader.Close()
				want := string(png)
				if i == 3 {
					want = "report bytes"
				}
				if string(data) != want {
					t.Fatal("uploaded bytes changed")
				}
			}
		})
	}
}

func TestMarkdownMediaBuffersIncompleteReferenceUntilFinal(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 2000, 1)
	store := stream.manager.attachmentStore.(*channeltest.MemoryAttachmentStore)
	store.SeedContainerFile("bot-1", "/data/report.txt", []byte("snapshot"), "text/plain", "report.txt")
	for _, delta := range []string{"before\n\n", "[report](/data/", "report.txt)\n\nafter"} {
		if err := stream.push(ctx, StreamEvent{Type: StreamEventDelta, Delta: delta}); err != nil {
			t.Fatal(err)
		}
	}
	if len(*sent) != 0 {
		t.Fatal("media sent before final")
	}
	for _, event := range reo.streams[0].events {
		if strings.Contains(event.Delta, "/data/") {
			t.Fatal("workspace reference leaked while streaming")
		}
	}
	final := boundFinal(stream, "before\n\n[report](/data/report.txt)\n\nafter")
	if err := stream.push(ctx, final); err != nil {
		t.Fatal(err)
	}
	if err := stream.push(ctx, final); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 || len((*sent)[0].Message.Attachments) != 1 || strings.TrimSpace((*sent)[1].Message.Text) != "after" {
		t.Fatalf("sent=%+v", *sent)
	}
}

func TestFailedMediaPreservesRemainingContent(t *testing.T) {
	msg := Message{Format: MessageFormatMarkdown, Text: "before ![missing](/data/missing.png) after"}
	msg = resolveMarkdownMessage(context.Background(), nil, ChannelConfig{BotID: "bot", ChannelType: ChannelTypeTelegram}, msg)
	parts, ok := expandMarkdownMessage(OutboundMessage{Message: msg})
	if !ok || len(parts) != 2 || parts[0].Message.Text != "before after" || !strings.Contains(parts[1].Message.Text, "could not be shared") {
		t.Fatalf("parts=%+v", parts)
	}
	if len(markdownmedia.Bindings(parts[1].Message.Metadata)) != 0 {
		t.Fatal("expanded references could be replayed")
	}
}

func TestMediaOnlyFinalUsesAttachmentSender(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 2000, 1)
	store := stream.manager.attachmentStore.(*channeltest.MemoryAttachmentStore)
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a2ioAAAAASUVORK5CYII=")
	store.SeedContainerFile("bot-1", "/data/chart.png", png, "image/png", "chart.png")
	final := boundFinal(stream, "![chart](/data/chart.png)")
	if err := stream.push(ctx, final); err != nil {
		t.Fatal(err)
	}
	if err := stream.push(ctx, final); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || len((*sent)[0].Message.Attachments) != 1 {
		t.Fatalf("sent=%+v", *sent)
	}
	// The adapter's open message is still completed, so no placeholder stays.
	if finals := adapterFinalTexts(reo); len(finals) != 1 || finals[0] != "" {
		t.Fatalf("adapter finals=%q", finals)
	}
}

func TestSplitMarkdownFinalPreservesWithheldProseWithoutRepeatingChunks(t *testing.T) {
	for _, prefixChunks := range []int{1, 3} {
		ctx := context.Background()
		stream, reo, sent := newDeltaSplitTestStream(t, 4096, prefixChunks+2)
		store := stream.manager.attachmentStore.(*channeltest.MemoryAttachmentStore)
		store.SeedContainerFile("bot-1", "/data/report.txt", []byte("snapshot"), "text/plain", "report.txt")
		prefixDelta := strings.Repeat("a", 3072) + "\n"
		prefix := strings.Repeat(prefixDelta, prefixChunks)
		tail := "Notice! retained tail\n\n[report](/data/report.txt)\n\nafter"
		for range prefixChunks {
			if err := stream.push(ctx, StreamEvent{Type: StreamEventDelta, Delta: prefixDelta}); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.push(ctx, StreamEvent{Type: StreamEventDelta, Delta: tail}); err != nil {
			t.Fatal(err)
		}
		final := boundFinal(stream, prefix+tail)
		if err := stream.push(ctx, final); err != nil {
			t.Fatal(err)
		}
		if err := stream.push(ctx, final); err != nil {
			t.Fatal(err)
		}
		var delivered strings.Builder
		for _, recording := range reo.streams {
			for _, event := range recording.events {
				delivered.WriteString(event.Delta)
				if event.Final != nil {
					delivered.WriteString(event.Final.Message.PlainText())
				}
			}
		}
		attachments := 0
		for _, part := range *sent {
			delivered.WriteString(part.Message.PlainText())
			attachments += len(part.Message.Attachments)
		}
		if !strings.Contains(delivered.String(), "Notice! retained tail") || strings.Count(delivered.String(), prefixDelta) != prefixChunks || attachments != 1 || strings.Count(delivered.String(), "after") != 1 {
			t.Fatalf("chunks=%d delivered=%q attachments=%d", prefixChunks, delivered.String(), attachments)
		}
	}
}

func TestMarkdownImageAndFileKeepDistinctDeliveryTypes(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a2ioAAAAASUVORK5CYII=")
	for _, source := range []string{
		"![preview](/data/a.png)\n\n[original](/data/a.png)\n\n[again](/data/a.png)",
		"[original](/data/a.png)\n\n![preview](/data/a.png)\n\n![again](/data/a.png)",
	} {
		store := channeltest.NewMemoryAttachmentStore()
		store.SeedContainerFile("bot-1", "/data/a.png", png, "image/png", "a.png")
		cfg := ChannelConfig{BotID: "bot-1", ChannelType: ChannelTypeTelegram}
		msg := resolveMarkdownMessage(context.Background(), store, cfg, Message{Text: source, Format: MessageFormatMarkdown})
		parts, _ := expandMarkdownMessage(OutboundMessage{Message: msg})
		types := map[AttachmentType]int{}
		for _, part := range parts {
			prepared, err := PrepareOutboundMessage(context.Background(), store, cfg, part)
			if err != nil {
				t.Fatal(err)
			}
			for _, attachment := range prepared.Message.Attachments {
				types[attachment.Logical.Type]++
			}
		}
		if types[AttachmentImage] != 1 || types[AttachmentFile] != 1 {
			t.Fatalf("source=%q types=%v", source, types)
		}
	}
}

func TestMarkdownFinalRetryClearsPendingText(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 1)
	store := stream.manager.attachmentStore.(*channeltest.MemoryAttachmentStore)
	store.SeedContainerFile("bot-1", "/data/report.txt", []byte("snapshot"), "text/plain", "report.txt")
	if err := stream.push(ctx, StreamEvent{Type: StreamEventDelta, Delta: "before [report](/data/report.txt) after"}); err != nil {
		t.Fatal(err)
	}
	send := stream.send
	fail := true
	stream.send = func(ctx context.Context, msg OutboundMessage) error {
		if fail {
			fail = false
			return errors.New("temporary transport failure")
		}
		return send(ctx, msg)
	}
	final := boundFinal(stream, "before [report](/data/report.txt) after")
	if err := stream.push(ctx, final); err == nil {
		t.Fatal("expected transport failure")
	}
	if err := stream.push(ctx, final); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || len((*sent)[0].Message.Attachments) != 1 {
		t.Fatalf("sent=%+v", *sent)
	}
	if stream.heldText.Len() != 0 || stream.carried().holding || len(stream.carried().segments) != 0 {
		t.Fatalf("withheld text outlived the delivered final: held=%q holding=%v", stream.heldText.String(), stream.carried().holding)
	}
	if finals := adapterFinalTexts(reo); len(finals) != 1 {
		t.Fatalf("adapter finals=%q", finals)
	}
}

func TestSplitMarkdownFinalWithFullyStreamedProse(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 2)
	store := stream.manager.attachmentStore.(*channeltest.MemoryAttachmentStore)
	store.SeedContainerFile("bot-1", "/data/report.txt", []byte("snapshot"), "text/plain", "report.txt")
	prefix := strings.Repeat("a", 3072) + "\n"
	if err := stream.push(ctx, StreamEvent{Type: StreamEventDelta, Delta: prefix}); err != nil {
		t.Fatal(err)
	}
	if stream.splitCount == 0 {
		t.Fatal("expected the prefix to split the stream")
	}
	if err := stream.push(ctx, boundFinal(stream, prefix+"[report](/data/report.txt)")); err != nil {
		t.Fatal(err)
	}
	// The link label stays in its paragraph. Only the unstreamed rest of it is
	// delivered, completing the message the split opened; the file follows.
	if len(*sent) != 1 || len((*sent)[0].Message.Attachments) != 1 {
		t.Fatalf("sent=%+v", *sent)
	}
	if finals := adapterFinalTexts(reo); finals[len(finals)-1] != "report" || strings.Contains(strings.Join(finals, ""), "aaa") {
		t.Fatalf("adapter finals=%q", finals)
	}
}

func TestUnsupportedAttachmentsDegradeToNotice(t *testing.T) {
	store := channeltest.NewMemoryAttachmentStore()
	store.SeedContainerFile("bot-1", "/data/report.txt", []byte("snapshot"), "text/plain", "report.txt")
	msg := resolveMarkdownMessage(context.Background(), store, ChannelConfig{BotID: "bot-1", ChannelType: ChannelTypeTelegram}, Message{Text: "[report](/data/report.txt)", Format: MessageFormatMarkdown})
	parts, _ := expandMarkdownMessage(OutboundMessage{Message: msg})
	built, err := buildExpandedMarkdownMessages(parts, OutboundPolicy{}, ChannelCapabilities{Text: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	last := built[len(built)-1].Message
	if len(last.Attachments) != 0 || !strings.Contains(last.Text, "could not be shared") {
		t.Fatalf("built=%+v", built)
	}
}

// boundFinal binds a final the way inbound does before Channel delivers it.
func boundFinal(stream *managerOutboundStream, text string) StreamEvent {
	msg := resolveMarkdownMessage(context.Background(), stream.manager.attachmentStore, stream.config, Message{Format: MessageFormatMarkdown, Text: text})
	return StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: msg}}
}

func adapterFinalTexts(reo *reopenablePreview) []string {
	var texts []string
	for _, recording := range reo.streams {
		for _, event := range recording.Events() {
			if event.Type == StreamEventFinal && event.Final != nil {
				texts = append(texts, strings.TrimSpace(event.Final.Message.PlainText()))
			}
		}
	}
	return texts
}

// endAtToolCall ends the stream's preview the way the delivery layer does when
// a tool call interrupts the text, and returns the stream that carries on the
// reply from the next preview.
func endAtToolCall(t *testing.T, prev *managerOutboundStream, reo *reopenablePreview) *managerOutboundStream {
	t.Helper()
	ctx := context.Background()
	if err := prev.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}}); err != nil {
		t.Fatal(err)
	}
	next, err := reo.reopen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &managerOutboundStream{
		manager: prev.manager, config: prev.config, preview: next, channelType: prev.channelType,
		target: prev.target, policy: prev.policy, send: prev.send, reopen: reo.reopen,
		carry: prev.carry, splitCount: 1, segmentSplits: 1,
	}
}

func pushDeltas(t *testing.T, stream *managerOutboundStream, deltas ...string) {
	t.Helper()
	for _, delta := range deltas {
		if err := stream.push(context.Background(), StreamEvent{Type: StreamEventDelta, Delta: delta}); err != nil {
			t.Fatal(err)
		}
	}
}

func streamedText(reo *reopenablePreview) string {
	var text strings.Builder
	for _, recording := range reo.streams {
		for _, event := range recording.Events() {
			text.WriteString(event.Delta)
		}
	}
	return text.String()
}

func TestOrdinaryMarkdownKeepsStreaming(t *testing.T) {
	stream, reo, _ := newDeltaSplitTestStream(t, 4096, 1)
	pushDeltas(t, stream, "Hello! ", "this ", "[site](https://example.com) ", "- [ ] task ", "is streamed")
	if got := streamedText(reo); got != "Hello! this [site](https://example.com) - [ ] task is streamed" {
		t.Fatalf("streamed=%q", got)
	}
}

func TestResetDiscardsWithheldText(t *testing.T) {
	stream, reo, _ := newDeltaSplitTestStream(t, 4096, 1)
	pushDeltas(t, stream, "Attempt one [x")
	if err := stream.push(context.Background(), StreamEvent{Type: StreamEventReset}); err != nil {
		t.Fatal(err)
	}
	pushDeltas(t, stream, "Second attempt")
	if err := stream.push(context.Background(), StreamEvent{Type: StreamEventPhaseEnd, Phase: StreamPhaseText}); err != nil {
		t.Fatal(err)
	}
	if got := streamedText(reo); got != "Attempt one Second attempt" {
		t.Fatalf("streamed=%q", got)
	}
}

func TestEarlierStepMediaIsPublishedFromItsSnapshot(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 2)
	pushDeltas(t, stream, "Created [report](/data/r.txt), now checking.")
	if err := stream.push(ctx, StreamEvent{Type: StreamEventPhaseEnd, Phase: StreamPhaseText}); err != nil {
		t.Fatal(err)
	}
	stream = endAtToolCall(t, stream, reo)
	pushDeltas(t, stream, "All tests pass.")
	ref := markdownmedia.Parse("[report](/data/r.txt)")[0]
	earlier := []markdownmedia.Binding{{Reference: ref, Asset: media.Asset{BotID: "bot-1", ContentHash: "snapshot-hash", Mime: "text/plain"}}}
	final := Message{Text: "All tests pass.", Format: MessageFormatMarkdown, Metadata: map[string]any{markdownmedia.EarlierMetadataKey: earlier}}
	if err := stream.push(ctx, StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: final}}); err != nil {
		t.Fatal(err)
	}
	if got := streamedText(reo); got != "Created " {
		t.Fatalf("streamed=%q", got)
	}
	// The first preview ends as it stands. The carried prose completes the
	// message of the preview that continues the reply; the rest follows.
	texts := adapterFinalTexts(reo)
	hash := ""
	for _, part := range *sent {
		if strings.Contains(part.Message.Text, "/data/") {
			t.Fatalf("workspace path leaked: %+v", part)
		}
		if _, ok := part.Message.Metadata[markdownmedia.EarlierMetadataKey]; ok {
			t.Fatalf("publication metadata reached the adapter: %+v", part)
		}
		if len(part.Message.Attachments) == 1 {
			hash = part.Message.Attachments[0].ContentHash
			texts = append(texts, "<file>")
			continue
		}
		texts = append(texts, strings.TrimSpace(part.Message.Text))
	}
	if strings.Join(texts, "|") != "|report, now checking.|<file>|All tests pass." || hash != "snapshot-hash" {
		t.Fatalf("sent=%q hash=%q", texts, hash)
	}
}

func TestReleasedTextRespectsChunkLimit(t *testing.T) {
	stream, reo, _ := newDeltaSplitTestStream(t, 100, 10)
	label := strings.Repeat("b", 300)
	pushDeltas(t, stream, "Hi [", label, "] end")
	if got := streamedText(reo); got != "Hi ["+label+"] end" {
		t.Fatalf("streamed=%q", got)
	}
	for _, recording := range reo.streams {
		for _, event := range recording.Events() {
			if runeLen(event.Delta) > 100 {
				t.Fatalf("delta over the platform limit: %d runes", runeLen(event.Delta))
			}
		}
	}
}

func TestLocalChannelsStreamMarkdownUnchanged(t *testing.T) {
	stream, reo, sent := newDeltaSplitTestStreamFor(t, "web", 4096, 1)
	text := "see ![a](/data/a.png) and [r](/data/r.txt)"
	pushDeltas(t, stream, text)
	if err := stream.push(context.Background(), StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: Message{Text: text, Format: MessageFormatMarkdown}}}); err != nil {
		t.Fatal(err)
	}
	if streamedText(reo) != text || len(*sent) != 0 {
		t.Fatalf("streamed=%q sent=%+v", streamedText(reo), *sent)
	}
	if msg := resolveMarkdownMessage(context.Background(), stream.manager.attachmentStore, ChannelConfig{BotID: "bot-1", ChannelType: "web"}, Message{Text: text, Format: MessageFormatMarkdown}); msg.Metadata != nil {
		t.Fatalf("local channel archived references: %+v", msg)
	}
}

func seedChart(stream *managerOutboundStream) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a2ioAAAAASUVORK5CYII=")
	stream.manager.attachmentStore.(*channeltest.MemoryAttachmentStore).SeedContainerFile("bot-1", "/data/c.png", png, "image/png", "c.png")
}

// Adapters may post a placeholder when the stream starts; a reply that opens
// with media must still complete that message with its prose.
func TestLeadingMediaCompletesTheAdapterMessageWithProse(t *testing.T) {
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 1)
	seedChart(stream)
	text := "![chart](/data/c.png)\n\nHere is the analysis."
	pushDeltas(t, stream, text)
	if err := stream.push(context.Background(), boundFinal(stream, text)); err != nil {
		t.Fatal(err)
	}
	if finals := adapterFinalTexts(reo); len(finals) != 1 || finals[0] != "Here is the analysis." {
		t.Fatalf("adapter finals=%q", finals)
	}
	if len(*sent) != 1 || len((*sent)[0].Message.Attachments) != 1 {
		t.Fatalf("sent=%+v", *sent)
	}
}

// Some runtimes repeat the text before a tool call as the final output. The
// final still has to reach the stream that continues the reply, and the
// repeated text is delivered once.
func TestRepeatedPreToolFinalDeliversCarriedMediaOnce(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 2)
	seedChart(stream)
	text := "Here ![c](/data/c.png)"
	pushDeltas(t, stream, text)
	continued := endAtToolCall(t, stream, reo)
	final := boundFinal(continued, text)
	snapshot := markdownmedia.Bindings(final.Final.Message.Metadata)
	final.Final.Message.Metadata[markdownmedia.EarlierMetadataKey] = snapshot
	if err := continued.push(ctx, final); err != nil {
		t.Fatal(err)
	}
	images := 0
	for _, part := range *sent {
		images += len(part.Message.Attachments)
		if strings.Contains(part.Message.Text, "Here") {
			t.Fatalf("repeated text delivered again: %+v", *sent)
		}
	}
	if images != 1 || streamedText(reo) != "Here " {
		t.Fatalf("images=%d streamed=%q sent=%+v", images, streamedText(reo), *sent)
	}
}

func TestPreToolMediaAfterUnclosedBacktickIsPublished(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 2)
	pushDeltas(t, stream, "Use the ` key. Chart: ![c](/data/c.png) next.")
	stream = endAtToolCall(t, stream, reo)
	pushDeltas(t, stream, "Done.")
	ref := markdownmedia.Parse("![c](/data/c.png)")[0]
	earlier := []markdownmedia.Binding{{Reference: ref, Asset: media.Asset{BotID: "bot-1", ContentHash: "step-one", Mime: "image/png"}}}
	final := Message{Text: "Done.", Format: MessageFormatMarkdown, Metadata: map[string]any{markdownmedia.EarlierMetadataKey: earlier}}
	if err := stream.push(ctx, StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: final}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(streamedText(reo), "/data/") {
		t.Fatalf("workspace path streamed: %q", streamedText(reo))
	}
	hash := ""
	for _, part := range *sent {
		if len(part.Message.Attachments) == 1 {
			hash = part.Message.Attachments[0].ContentHash
		}
	}
	if hash != "step-one" {
		t.Fatalf("sent=%+v", *sent)
	}
}

// Carried text is parsed in the context of its whole segment, so a held
// image inside an indented code block stays code, without a failure notice.
func TestCarriedSegmentKeepsCodeAsCode(t *testing.T) {
	ctx := context.Background()
	stream, reo, sent := newDeltaSplitTestStream(t, 4096, 2)
	seedChart(stream)
	pushDeltas(t, stream, "Real: ![c](/data/c.png)\n\nExample:\n\n    ![a](/data/a.png)\n\nnow running.")
	stream = endAtToolCall(t, stream, reo)
	ref := markdownmedia.Parse("![c](/data/c.png)")[0]
	earlier := []markdownmedia.Binding{{Reference: ref, Asset: media.Asset{BotID: "bot-1", ContentHash: "real", Mime: "image/png"}}}
	final := Message{Text: "Done.", Format: MessageFormatMarkdown, Metadata: map[string]any{markdownmedia.EarlierMetadataKey: earlier}}
	if err := stream.push(ctx, StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: final}}); err != nil {
		t.Fatal(err)
	}
	var delivered strings.Builder
	delivered.WriteString(strings.Join(adapterFinalTexts(reo), "|"))
	for _, part := range *sent {
		delivered.WriteString("|" + part.Message.Text)
	}
	if strings.Contains(delivered.String(), "could not be shared") || !strings.Contains(delivered.String(), "    ![a](/data/a.png)") {
		t.Fatalf("delivered=%q", delivered.String())
	}
}

func TestFenceOpenedInEarlierDeltaStreamsAfterResume(t *testing.T) {
	stream, reo, _ := newDeltaSplitTestStream(t, 4096, 1)
	pushDeltas(t, stream, "Intro [site](https://example.com)\n```md\n", "![a](/data/a.png)\n", "```\nafter")
	if got := streamedText(reo); got != "Intro [site](https://example.com)\n```md\n![a](/data/a.png)\n```\nafter" {
		t.Fatalf("streamed=%q", got)
	}
}

// Channel delivers references the agent already bound; it never archives an
// unbound message itself (for example one posted through the HTTP send API).
func TestManagerSendDeliversUnboundMarkdownAsWritten(t *testing.T) {
	channelType := ChannelType("target-resolving-send")
	adapter := &targetResolvingAdapter{channelType: channelType}
	registry := NewRegistry()
	if err := registry.Register(adapter); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, registry, &fakeConfigStore{effectiveConfig: ChannelConfig{BotID: "bot-1", ChannelType: channelType}}, nil)
	store := channeltest.NewMemoryAttachmentStore()
	store.SeedContainerFile("bot-1", "/data/report.txt", []byte("report"), "text/plain", "report.txt")
	manager.SetAttachmentStore(store)
	msg := WithoutMarkdownMedia(Message{Text: "see [report](/data/report.txt)", Format: MessageFormatMarkdown, Metadata: map[string]any{markdownmedia.MetadataKey: []markdownmedia.Binding{{}}}})
	if _, ok := msg.Metadata[markdownmedia.MetadataKey]; ok {
		t.Fatal("caller-supplied publication metadata kept")
	}
	if err := manager.Send(context.Background(), "bot-1", channelType, SendRequest{Target: "alias", Message: msg}); err != nil {
		t.Fatal(err)
	}
	if len(adapter.sent) != 1 || len(adapter.sent[0].Message.Attachments) != 0 {
		t.Fatalf("sent=%+v", adapter.sent)
	}
}

// A tool call ends the reply's preview and a later preview carries on. The
// reference written before the call is published with the final, in its place
// in the text, and what the carrying preview is given after it waits as well.
func TestMediaWithheldBeforeAToolCallIsPublishedByTheContinuingPreview(t *testing.T) {
	ctx := context.Background()
	adapter := newPreviewAdapter("preview-media", 0)
	surface := newPreviewSurface(t, adapter)
	store := surface.manager.attachmentStore.(*channeltest.MemoryAttachmentStore)
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a2ioAAAAASUVORK5CYII=")
	store.SeedContainerFile("bot-1", "/data/c.png", png, "image/png", "c.png")

	first, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := "Here ![c](/data/c.png) is the chart."
	if err := first.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: before}); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}}); err != nil {
		t.Fatal(err)
	}
	second, err := surface.OpenPreview(ctx, "target-1", StreamOptions{Continuation: true, After: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "Checking."}); err != nil {
		t.Fatal(err)
	}
	snapshot := resolveMarkdownMessage(ctx, store, ChannelConfig{BotID: "bot-1", ChannelType: adapter.channelType}, Message{Format: MessageFormatMarkdown, Text: before})
	final := Message{Format: MessageFormatMarkdown, Text: "Checking.", Metadata: map[string]any{markdownmedia.EarlierMetadataKey: snapshot.Metadata[markdownmedia.MetadataKey]}}
	if err := second.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{Message: final}}); err != nil {
		t.Fatal(err)
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	var streamed strings.Builder
	for _, update := range adapter.updates {
		streamed.WriteString(update.LogicalEvent().Delta)
	}
	if streamed.String() != "Here " {
		t.Fatalf("streamed=%q, want only the text before the reference", streamed.String())
	}
	// Finals first, then the messages sent besides them: the first preview ends
	// as it stands, and the second completes its message with the carried prose.
	var delivered []string
	for _, final := range adapter.finals {
		if final != nil {
			delivered = append(delivered, strings.TrimSpace(final.Final.Message.LogicalMessage().PlainText()))
		}
	}
	for _, msg := range adapter.messages {
		if len(msg.Attachments) > 0 {
			delivered = append(delivered, "<image>")
			continue
		}
		delivered = append(delivered, strings.TrimSpace(msg.PlainText()))
	}
	if got := strings.Join(delivered, "|"); got != "|is the chart.|<image>|Checking." {
		t.Fatalf("delivered=%q", got)
	}
}

// Everything after a withheld reference waits for the final, also in the
// previews that continue the reply, so the media keeps its place in the text.
func TestTextAfterWithheldMediaWaitsInTheContinuingPreview(t *testing.T) {
	ctx := context.Background()
	adapter := newPreviewAdapter("preview-hold", 0)
	surface := newPreviewSurface(t, adapter)
	first, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "![c](/data/c.png)"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}}); err != nil {
		t.Fatal(err)
	}
	second, err := surface.OpenPreview(ctx, "target-1", StreamOptions{Continuation: true, After: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "after the call"}); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	for _, update := range adapter.updates {
		if update.LogicalEvent().Delta != "" {
			t.Fatalf("text streamed ahead of the withheld media: %+v", update.LogicalEvent())
		}
	}
}

// What a reply withheld belongs to that reply: a failed reply does not show a
// half-written reference, and the next reply starts clean.
func TestWithheldMediaDoesNotOutliveItsReply(t *testing.T) {
	ctx := context.Background()
	adapter := newPreviewAdapter("preview-abort", 0)
	surface := newPreviewSurface(t, adapter)
	first, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "Look ![c](/data/c"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "Fresh reply"}); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	var streamed strings.Builder
	for _, update := range adapter.updates {
		streamed.WriteString(update.LogicalEvent().Delta)
	}
	if streamed.String() != "Look Fresh reply" {
		t.Fatalf("streamed=%q", streamed.String())
	}
}

// Two replies one after the other on a surface, as a queued answer is followed
// by the next reply, do not share what the first withheld: the second is not
// opened as a continuation, so it starts with nothing held.
func TestReplyDoesNotInheritMediaWithheldByThePreviousReply(t *testing.T) {
	ctx := context.Background()
	adapter := newPreviewAdapter("preview-fresh", 0)
	surface := newPreviewSurface(t, adapter)
	first, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "![c](/data/c.png)"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}}); err != nil {
		t.Fatal(err)
	}
	next, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "Fresh reply"}); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	var streamed strings.Builder
	for _, update := range adapter.updates {
		streamed.WriteString(update.LogicalEvent().Delta)
	}
	if streamed.String() != "Fresh reply" {
		t.Fatalf("streamed=%q, want the new reply shown without waiting for the previous reply's media", streamed.String())
	}
}

// A continuation takes the state from the preview it is given as the one it
// carries on from, and from no other.
func TestContinuationTakesTheStateOfThePreviewItCarriesOn(t *testing.T) {
	ctx := context.Background()
	adapter := newPreviewAdapter("preview-after", 0)
	surface := newPreviewSurface(t, adapter)
	held, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Update(ctx, StreamEvent{Type: StreamEventDelta, Delta: "![c](/data/c.png)"}); err != nil {
		t.Fatal(err)
	}
	if err := held.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}}); err != nil {
		t.Fatal(err)
	}
	other, err := surface.OpenPreview(ctx, "target-1", StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Finish(ctx, &StreamEvent{Type: StreamEventFinal, Final: &StreamFinalizePayload{}}); err != nil {
		t.Fatal(err)
	}
	carried, err := surface.OpenPreview(ctx, "target-1", StreamOptions{Continuation: true, After: held})
	if err != nil {
		t.Fatal(err)
	}
	if got := carried.(*managerOutboundStream).carry; !got.holding || len(got.segments) == 0 {
		t.Fatalf("carry=%+v, want the state of the preview it carries on from", got)
	}
	unrelated, err := surface.OpenPreview(ctx, "target-1", StreamOptions{Continuation: true, After: other})
	if err != nil {
		t.Fatal(err)
	}
	if got := unrelated.(*managerOutboundStream).carry; got.holding || len(got.segments) != 0 {
		t.Fatalf("carry=%+v, want nothing withheld", got)
	}
}
