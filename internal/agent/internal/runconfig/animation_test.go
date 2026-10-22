package runconfig

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/models"
)

// The failure #1232 shipped: expanding frames into separate attachments makes
// one sticker look like five to everything downstream. The attachment has to
// stay singular — it is one file, with one path, counted once — and fan out
// only where images are handed to the model.
func TestAnimatedStickerStaysOneAttachmentWithManyImages(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"sticker": agenttest.MovingStickerTGS(t)})
	req := runreq.ChatRequest{
		BotID:       "bot-1",
		Attachments: []runreq.ChatAttachment{{Type: "image", ContentHash: "sticker", Mime: "image/webp", Name: "sticker.tgs"}},
	}
	model := models.GetResponse{Model: models.Model{Config: models.ModelConfig{
		Compatibilities: []string{models.CompatVision},
	}}}

	merged := s.routeAndMergeAttachments(context.Background(), model, req)
	if len(merged) != 1 {
		t.Fatalf("routeAndMergeAttachments() = %d attachments, want the sticker counted once", len(merged))
	}
	if paths := extractAttachmentPaths(merged); len(paths) != 1 {
		t.Fatalf("extractAttachmentPaths() = %v, want a single reference to the original", paths)
	}

	item, ok := merged[0].(GatewayAttachment)
	if !ok || item.Type != "image" || item.Mime != "image/png" {
		t.Fatalf("routed attachment = %#v, want a PNG image attachment", merged[0])
	}
	if len(item.Frames) != AnimationFrameCount {
		t.Fatalf("Frames = %d, want %d rendered frames", len(item.Frames), AnimationFrameCount)
	}
	if item.Payload != item.Frames[0] {
		t.Fatal("Payload must stay the first frame so routing and size checks see a real image")
	}
	// The budget has to see every frame the model will receive, not just one.
	if want := totalInlineDataURLBytes(item.Frames); item.Size != want {
		t.Fatalf("Size = %d, want %d — the whole frame set", item.Size, want)
	}

	parts := extractNativeImageParts(merged)
	if len(parts) != AnimationFrameCount {
		t.Fatalf("extractNativeImageParts() = %d parts, want %d", len(parts), AnimationFrameCount)
	}
	for i, part := range parts {
		if part.MediaType != "image/png" || !strings.HasPrefix(part.Image, "data:image/png;base64,") {
			t.Fatalf("part %d = %#v, want a PNG data URL", i, part)
		}
	}
	// Distinct frames, not the same image five times.
	unique := map[string]bool{}
	for _, part := range parts {
		unique[part.Image] = true
	}
	if len(unique) != AnimationFrameCount {
		t.Fatalf("%d distinct frames out of %d — the sampled frames are not distinct", len(unique), len(parts))
	}
}

// Every entry point has to see the same frames; that divergence is what let
// the original bug reach the model from one path and not another.
func TestAnimatedStickerIsIdenticalAcrossEntryPoints(t *testing.T) {
	data := agenttest.MovingStickerTGS(t)
	s := imageInputService(t, map[string][]byte{"sticker": data})
	req := runreq.ChatRequest{
		BotID:       "bot-1",
		Attachments: []runreq.ChatAttachment{{Type: "image", ContentHash: "sticker", Mime: "application/x-gzip"}},
	}
	model := models.GetResponse{Model: models.Model{Config: models.ModelConfig{
		Compatibilities: []string{models.CompatVision},
	}}}

	gateway := extractNativeImageParts(s.routeAndMergeAttachments(context.Background(), model, req))
	injected := s.InlineInjectAttachments(context.Background(), "bot-1", req.Attachments)

	if len(gateway) != AnimationFrameCount || len(injected) != len(gateway) {
		t.Fatalf("frame counts differ: gateway=%d injected=%d", len(gateway), len(injected))
	}
	for i := range gateway {
		if injected[i].Image != gateway[i].Image {
			t.Fatalf("frame %d differs between entry points", i)
		}
	}
}

// A sticker that never moves is five copies of one picture. Sending it five
// times costs five times as much and says nothing more.
func TestStaticAnimationCollapsesToOneFrame(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"still": agenttest.TGSSticker(t)})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1",
		[]runreq.ChatAttachment{{Type: "image", ContentHash: "still", Mime: "application/x-gzip"}})
	if len(parts) != 1 {
		t.Fatalf("inlineInjectAttachments() = %d parts, want identical frames collapsed to one", len(parts))
	}
}

// Stickers are drawn for a chat background; an unflattened frame reaches some
// providers as a subject on black.
func TestAnimatedStickerFramesAreFlattenedOnWhite(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"sticker": agenttest.MovingStickerTGS(t)})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1",
		[]runreq.ChatAttachment{{Type: "image", ContentHash: "sticker", Mime: "application/x-gzip"}})
	if len(parts) == 0 {
		t.Fatal("no frames rendered")
	}
	frame := agenttest.DecodeDataURLImage(t, parts[0].Image)
	// Top-right corner is outside the square at every sampled frame.
	r, g, b, a := frame.At(frame.Bounds().Dx()-1, 0).RGBA()
	if r != 0xffff || g != 0xffff || b != 0xffff || a != 0xffff {
		t.Fatalf("corner pixel = (%d,%d,%d,%d), want opaque white", r, g, b, a)
	}
}

// A render failure must not take the turn's other images down with it.
func TestAnimationFailureLeavesOtherImagesAlone(t *testing.T) {
	s := imageInputService(t, map[string][]byte{
		// gzip magic, but nothing behind it that ThorVG can parse.
		"broken": append([]byte{0x1f, 0x8b, 0x08}, bytes.Repeat([]byte{0x00}, 64)...),
		"photo":  agenttest.RasterPNG(t),
	})
	s.logger = slog.Default()
	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		{Type: "image", ContentHash: "broken", Mime: "application/x-gzip"},
		{Type: "image", ContentHash: "photo", Mime: "image/png"},
	})
	if len(parts) != 1 || parts[0].Image != agenttest.DataURL("image/png", agenttest.RasterPNG(t)) {
		t.Fatalf("inlineInjectAttachments() = %#v, want only the intact photo", parts)
	}
}

// The Telegram adapter now labels an animated sticker application/x-tgsticker
// and stores the Lottie itself. That label is not one the raster allowlist
// knows, so this is the regression guard that the two layers still agree: the
// bytes decide, and the sticker reaches the model as frames.
func TestAdapterLabelledAnimatedStickerRenders(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"sticker": agenttest.MovingStickerTGS(t)})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1",
		[]runreq.ChatAttachment{{Type: "image", ContentHash: "sticker", Mime: "application/x-tgsticker"}})
	if len(parts) != AnimationFrameCount {
		t.Fatalf("inlineInjectAttachments() = %d parts, want %d frames", len(parts), AnimationFrameCount)
	}
}
