package runconfig

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/models"
)

func imageInputService(t *testing.T, assets map[string][]byte) *Builder {
	t.Helper()
	return newBuilder(func(d *Deps) { d.Assets = agenttest.ImageAssets(t, assets) })
}

// The reported failure: an unusable attachment sitting in pending discussion
// context made every later turn fail image parsing. It must be skipped without
// taking the other images of the turn down with it.
func TestInlineInjectAttachmentsRendersAnimationsAndSkipsUnparsableBytes(t *testing.T) {
	s := imageInputService(t, map[string][]byte{
		"video-sticker":    agenttest.WebMSticker(),
		"animated-sticker": agenttest.MovingStickerTGS(t),
		"photo":            agenttest.RasterPNG(t),
	})

	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		// WebM has no renderer here, so it stays out of the vision lane.
		{Type: "image", ContentHash: "video-sticker", Mime: "video/webm"},
		// TGS is rendered into frames, whatever the stored label claims.
		{Type: "image", ContentHash: "animated-sticker", Mime: "application/x-gzip"},
		{Type: "image", ContentHash: "photo", Mime: "image/png"},
	})

	if len(parts) != AnimationFrameCount+1 {
		t.Fatalf("inlineInjectAttachments() = %d parts, want %d sticker frames plus the photo",
			len(parts), AnimationFrameCount)
	}
	for i, part := range parts {
		if part.MediaType != "image/png" || !strings.HasPrefix(part.Image, "data:image/png;base64,") {
			t.Fatalf("part %d = %#v, want a PNG data URL", i, part)
		}
	}
	if parts[len(parts)-1].Image != agenttest.DataURL("image/png", agenttest.RasterPNG(t)) {
		t.Fatal("the ordinary photo was not passed through untouched")
	}
}

// Historical records label sticker bytes as image/png. The label must not be
// enough to get them into a vision request.
func TestInlineInjectAttachmentsIgnoresMislabeledStickerBytes(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"legacy": agenttest.WebMSticker()})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		{Type: "image", ContentHash: "legacy", Mime: "image/png"},
	})
	if len(parts) != 0 {
		t.Fatalf("inlineInjectAttachments() = %#v, want nothing for WebM bytes labelled image/png", parts)
	}
}

func TestInlineInjectAttachmentsLoadsEachAssetOnce(t *testing.T) {
	opens := 0
	s := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
				opens++
				return io.NopCloser(bytes.NewReader(agenttest.RasterPNG(t))), "image/png", nil
			},
		}
	})
	refs := []runreq.ChatAttachment{
		{Type: "image", ContentHash: "photo", Mime: "image/png"},
		{Type: "image", ContentHash: "photo", Mime: "image/png"},
	}
	if parts := s.InlineInjectAttachments(context.Background(), "bot-1", refs); len(parts) != 1 {
		t.Fatalf("inlineInjectAttachments() = %d parts, want the repeated reference collapsed", len(parts))
	}
	if opens != 1 {
		t.Fatalf("asset opened %d times, want 1", opens)
	}
}

func TestInlineInjectAttachmentsRendersAnimations(t *testing.T) {
	s := imageInputService(t, map[string][]byte{
		"animated-sticker": agenttest.MovingStickerTGS(t),
		"video-sticker":    agenttest.WebMSticker(),
	})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		{Type: "image", ContentHash: "animated-sticker", Mime: "image/webp"},
		{Type: "image", ContentHash: "video-sticker", Mime: "image/png"},
	})
	if len(parts) != AnimationFrameCount {
		t.Fatalf("inlineInjectAttachments() = %d parts, want %d frames from the animation and nothing from the WebM",
			len(parts), AnimationFrameCount)
	}
}

// Ordinary chat already refused these bytes at the capability router. It must
// keep doing so through the file lane, never as a vision part.
func TestGatewayStickerBytesRouteToFileFallback(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"video-sticker": agenttest.WebMSticker()})
	req := runreq.ChatRequest{
		BotID:       "bot-1",
		Attachments: []runreq.ChatAttachment{{Type: "image", ContentHash: "video-sticker", Mime: "image/png"}},
	}
	model := models.GetResponse{Model: models.Model{Config: models.ModelConfig{
		Compatibilities: []string{models.CompatVision},
	}}}

	merged := s.routeAndMergeAttachments(context.Background(), model, req)
	if len(merged) != 1 {
		t.Fatalf("routeAndMergeAttachments() = %d attachments, want 1", len(merged))
	}
	item, ok := merged[0].(GatewayAttachment)
	if !ok || item.Type != "file" || item.Transport != gatewayTransportToolFileRef {
		t.Fatalf("routed attachment = %#v, want a file reference", merged[0])
	}
	if item.Payload != "/data/.memoh/media/video-sticker" {
		t.Fatalf("file reference = %q, want the stored original", item.Payload)
	}
	if parts := extractNativeImageParts(merged); len(parts) != 0 {
		t.Fatalf("extractNativeImageParts() = %#v, want no vision input", parts)
	}
}

// An upload is bytes too: a renamed video must not reach the model just because
// the browser announced it as a PNG.
func TestGatewayInlineDataURLWithNonRasterBytesIsWithheld(t *testing.T) {
	s := newBuilder()
	item := GatewayAttachment{
		Type:      "image",
		Mime:      "image/png",
		Transport: GatewayTransportInlineDataURL,
		Payload:   agenttest.DataURL("image/png", agenttest.WebMSticker()),
	}
	got := s.inlineImageAttachmentAssetIfNeeded(context.Background(), "bot-1", item)
	if IsNativeImageAttachment(got) {
		t.Fatalf("attachment = %#v, want it demoted out of the vision lane", got)
	}
}

// A wrong subtype is a provider error of its own, and the correction has to
// reach the data URL: the payload and the MIME field travel to different
// consumers from here on.
func TestGatewayInlineDataURLRestampsMislabeledSubtype(t *testing.T) {
	s := newBuilder()
	data := agenttest.RasterPNG(t)
	item := GatewayAttachment{
		Type:      "image",
		Mime:      "image/jpeg",
		Transport: GatewayTransportInlineDataURL,
		Payload:   agenttest.DataURL("image/jpeg", data),
	}
	got := s.inlineImageAttachmentAssetIfNeeded(context.Background(), "bot-1", item)
	if got.Mime != "image/png" || got.Payload != agenttest.DataURL("image/png", data) {
		t.Fatalf("attachment = %#v, want both the MIME and the data URL corrected to image/png", got)
	}
	parts := extractNativeImageParts([]any{got})
	if len(parts) != 1 || parts[0].MediaType != "image/png" ||
		!strings.HasPrefix(parts[0].Image, "data:image/png;base64,") {
		t.Fatalf("image part = %#v, want a consistent image/png", parts)
	}
}

// dripReader returns one byte per call: a legal Reader, and the shape a short
// read takes when the media store is not a plain file.
type dripReader struct{ data []byte }

func (d *dripReader) Read(p []byte) (int, error) {
	if len(d.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = d.data[0]
	d.data = d.data[1:]
	return 1, nil
}

func TestStoredImageSurvivesShortReads(t *testing.T) {
	data := agenttest.RasterPNG(t)
	s := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
				return io.NopCloser(&dripReader{data: bytes.Clone(data)}), "image/png", nil
			},
		}
	})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		{Type: "image", ContentHash: "photo", Mime: "image/png"},
	})
	if len(parts) != 1 || parts[0].MediaType != "image/png" {
		t.Fatalf("inlineInjectAttachments() = %#v, want the image through a drip-feeding reader", parts)
	}
	if parts[0].Image != agenttest.DataURL("image/png", data) {
		t.Fatal("drip-feeding reader truncated the encoded image")
	}
}

// An image shorter than the sniff window still has to be recognised.
func TestStoredImageShorterThanSniffWindow(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01, 0xFF, 0xD9}
	s := imageInputService(t, map[string][]byte{"tiny": jpegBytes})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		{Type: "image", ContentHash: "tiny", Mime: "image/jpeg"},
	})
	if len(parts) != 1 || parts[0].Image != agenttest.DataURL("image/jpeg", jpegBytes) {
		t.Fatalf("inlineInjectAttachments() = %#v, want the whole short image", parts)
	}
}

func TestGatewayInlineDataURLKeepsRealImage(t *testing.T) {
	s := newBuilder()
	item := GatewayAttachment{
		Type:      "image",
		Mime:      "image/png",
		Transport: GatewayTransportInlineDataURL,
		Payload:   agenttest.DataURL("image/png", agenttest.RasterPNG(t)),
	}
	got := s.inlineImageAttachmentAssetIfNeeded(context.Background(), "bot-1", item)
	if got.Mime != "image/png" || got.Payload != item.Payload || !IsNativeImageAttachment(got) {
		t.Fatalf("attachment = %#v, want it untouched and still native", got)
	}
}

// A wrong subtype is its own source of provider 400s, and the bytes know better.
func TestStoredImageMimeIsCorrectedFromBytes(t *testing.T) {
	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01, 0xFF, 0xD9}
	s := imageInputService(t, map[string][]byte{"mislabeled": jpegBytes})
	parts := s.InlineInjectAttachments(context.Background(), "bot-1", []runreq.ChatAttachment{
		{Type: "image", ContentHash: "mislabeled", Mime: "image/png"},
	})
	if len(parts) != 1 || parts[0].MediaType != "image/jpeg" {
		t.Fatalf("inlineInjectAttachments() = %#v, want the sniffed image/jpeg", parts)
	}
}

func TestEncodeReaderAsDataURLRejectsOversizedImage(t *testing.T) {
	data := agenttest.RasterPNG(t)
	_, _, err := encodeReaderAsDataURL(bytes.NewReader(data), int64(len(data)-1), "image", "image/png")
	if err == nil || !strings.Contains(err.Error(), "asset too large to inline") {
		t.Fatalf("encodeReaderAsDataURL() error = %v, want the size guard", err)
	}
}
