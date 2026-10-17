package runconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/models"
)

func TestPrepareGatewayAttachments_InlineAssetToBase64(t *testing.T) {
	resolver := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(_ context.Context, _, contentHash string) (io.ReadCloser, string, error) {
				if contentHash != "asset-1" {
					t.Fatalf("unexpected content hash: %s", contentHash)
				}
				return io.NopCloser(bytes.NewReader(agenttest.RasterPNG(t))), "image/png", nil
			},
		}
	})
	req := runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{
			{
				Type:        "image",
				ContentHash: "asset-1",
			},
		},
	}

	prepared := resolver.PrepareGatewayAttachments(context.Background(), req, true)
	if len(prepared) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(prepared))
	}
	if prepared[0].Transport != GatewayTransportInlineDataURL {
		t.Fatalf("expected inline transport, got %q", prepared[0].Transport)
	}
	if !strings.HasPrefix(prepared[0].Payload, "data:image/png;base64,") {
		t.Fatalf("expected data url image attachment, got %q", prepared[0].Payload)
	}
	if prepared[0].Mime != "image/png" {
		t.Fatalf("expected mime image/png, got %q", prepared[0].Mime)
	}
}

func TestPrepareGatewayAttachments_DataURLFromURLFieldIsNativeInline(t *testing.T) {
	resolver := newBuilder()
	req := runreq.ChatRequest{
		Attachments: []runreq.ChatAttachment{
			{
				Type: "image",
				URL:  "data:image/png;base64,AAAA",
			},
		},
	}

	prepared := resolver.PrepareGatewayAttachments(context.Background(), req, true)
	if len(prepared) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(prepared))
	}
	if prepared[0].Transport != GatewayTransportInlineDataURL {
		t.Fatalf("expected inline transport, got %q", prepared[0].Transport)
	}
	if prepared[0].Payload != "data:image/png;base64,AAAA" {
		t.Fatalf("unexpected payload: %q", prepared[0].Payload)
	}
	if prepared[0].FallbackPath != "" {
		t.Fatalf("expected empty fallback path, got %q", prepared[0].FallbackPath)
	}
}

func TestPrepareGatewayAttachments_PublicURLFromURLFieldIsNativePublic(t *testing.T) {
	resolver := newBuilder()
	req := runreq.ChatRequest{
		Attachments: []runreq.ChatAttachment{
			{
				Type: "image",
				URL:  "https://example.com/demo.png",
			},
		},
	}

	prepared := resolver.PrepareGatewayAttachments(context.Background(), req, true)
	if len(prepared) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(prepared))
	}
	if prepared[0].Transport != GatewayTransportPublicURL {
		t.Fatalf("expected public transport, got %q", prepared[0].Transport)
	}
	if prepared[0].Payload != "https://example.com/demo.png" {
		t.Fatalf("unexpected payload: %q", prepared[0].Payload)
	}
	if prepared[0].FallbackPath != "" {
		t.Fatalf("expected empty fallback path, got %q", prepared[0].FallbackPath)
	}
}

func TestRouteAndMergeAttachments_ImagePathOnlyFallsBackToFile(t *testing.T) {
	resolver := newBuilder()
	model := models.GetResponse{
		Model: models.Model{
			Config: models.ModelConfig{
				Compatibilities: []string{models.CompatVision},
			},
		},
	}
	req := runreq.ChatRequest{
		Attachments: []runreq.ChatAttachment{
			{
				Type: "image",
				Path: "/data/.memoh/media/image/demo.png",
			},
		},
	}

	merged := resolver.routeAndMergeAttachments(context.Background(), model, req)
	if len(merged) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(merged))
	}
	item, ok := merged[0].(GatewayAttachment)
	if !ok {
		t.Fatalf("expected gatewayAttachment type")
	}
	if item.Type != "file" {
		t.Fatalf("expected fallback type file, got %q", item.Type)
	}
	if item.Transport != gatewayTransportToolFileRef {
		t.Fatalf("expected tool_file_ref transport, got %q", item.Transport)
	}
	if item.Payload != "/data/.memoh/media/image/demo.png" {
		t.Fatalf("unexpected fallback payload: %q", item.Payload)
	}
}

func TestPrepareGatewayAttachments_IncludesReplyAttachments(t *testing.T) {
	resolver := newBuilder()
	req := runreq.ChatRequest{
		Attachments: []runreq.ChatAttachment{
			{Type: "image", URL: "https://example.com/current.png"},
		},
		ReplyAttachments: []runreq.ChatAttachment{
			{Type: "image", URL: "https://example.com/reply.png"},
		},
	}

	prepared := resolver.PrepareGatewayAttachments(context.Background(), req, true)
	if len(prepared) != 2 {
		t.Fatalf("expected current and reply attachments, got %d", len(prepared))
	}
	if prepared[0].Payload != "https://example.com/current.png" || prepared[1].Payload != "https://example.com/reply.png" {
		t.Fatalf("unexpected prepared attachments: %#v", prepared)
	}
}

func TestPrepareGatewayAttachments_ResolvesStoredFileAccessPath(t *testing.T) {
	t.Parallel()

	resolver := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			AccessPathFn: func(_ context.Context, botID, contentHash string) (string, error) {
				if botID != "bot-1" || contentHash != "asset-pdf" {
					t.Fatalf("unexpected asset lookup: bot=%q hash=%q", botID, contentHash)
				}
				return "/data/.memoh/media/aa/asset.pdf", nil
			},
		}
	})
	req := runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "file",
			Mime:        "application/pdf",
			ContentHash: "asset-pdf",
		}},
	}

	prepared := resolver.PrepareGatewayAttachments(context.Background(), req, true)
	if len(prepared) != 1 || prepared[0].FallbackPath != "/data/.memoh/media/aa/asset.pdf" {
		t.Fatalf("prepared attachments = %#v, want reachable PDF path", prepared)
	}
	merged := resolver.routeAndMergeAttachments(context.Background(), models.GetResponse{}, req)
	if len(merged) != 1 {
		t.Fatalf("routeAndMergeAttachments() length = %d, want 1", len(merged))
	}
	item, ok := merged[0].(GatewayAttachment)
	if !ok || item.Transport != gatewayTransportToolFileRef || item.Payload != "/data/.memoh/media/aa/asset.pdf" {
		t.Fatalf("merged attachment = %#v, want tool file reference", merged[0])
	}
}

func TestPrepareGatewayAttachments_DetectsImageMimeWhenOctetStream(t *testing.T) {
	jpegBytes := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46,
		0x49, 0x46, 0x00, 0x01, 0xFF, 0xD9,
	}
	resolver := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(_ context.Context, _, _ string) (io.ReadCloser, string, error) {
				return io.NopCloser(bytes.NewReader(jpegBytes)), "application/octet-stream", nil
			},
		}
	})
	req := runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{
			{
				Type:        "image",
				ContentHash: "asset-2",
			},
		},
	}

	prepared := resolver.PrepareGatewayAttachments(context.Background(), req, true)
	if len(prepared) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(prepared))
	}
	if prepared[0].Transport != GatewayTransportInlineDataURL {
		t.Fatalf("expected inline transport, got %q", prepared[0].Transport)
	}
	if !strings.HasPrefix(prepared[0].Payload, "data:image/jpeg;base64,") {
		t.Fatalf("expected detected image/jpeg data url, got %q", prepared[0].Payload)
	}
	if prepared[0].Mime != "image/jpeg" {
		t.Fatalf("expected mime image/jpeg, got %q", prepared[0].Mime)
	}
}

func TestRouteAndMergeAttachments_DropsUnsupportedInlineWithoutFallbackPath(t *testing.T) {
	resolver := newBuilder()
	model := models.GetResponse{
		Model: models.Model{
			Config: models.ModelConfig{
				Compatibilities: []string{},
			},
		},
	}
	req := runreq.ChatRequest{
		Attachments: []runreq.ChatAttachment{
			{
				Type:   "video",
				Base64: "AAAA",
			},
		},
	}

	merged := resolver.routeAndMergeAttachments(context.Background(), model, req)
	if len(merged) != 0 {
		t.Fatalf("expected unsupported inline attachment to be dropped, got %d", len(merged))
	}
}

func TestEncodeReaderAsDataURL_DetectsImageMime(t *testing.T) {
	jpegBytes := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46,
		0x49, 0x46, 0x00, 0x01, 0xFF, 0xD9,
	}

	dataURL, mime, err := encodeReaderAsDataURL(
		bytes.NewReader(jpegBytes),
		int64(len(jpegBytes)),
		"image",
		"application/octet-stream",
	)
	if err != nil {
		t.Fatalf("encodeReaderAsDataURL returned error: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("expected image/jpeg mime, got %q", mime)
	}
	expected := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes)
	if dataURL != expected {
		t.Fatalf("unexpected data URL")
	}
}

func TestEncodeReaderAsDataURL_RejectsOversizedPayload(t *testing.T) {
	_, _, err := encodeReaderAsDataURL(strings.NewReader("12345"), 4, "file", "text/plain")
	if err == nil {
		t.Fatal("expected error for oversized payload")
	}
	if !strings.Contains(err.Error(), "asset too large to inline") {
		t.Fatalf("unexpected error: %v", err)
	}
}
