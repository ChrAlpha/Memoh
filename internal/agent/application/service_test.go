package application

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/server"
)

func TestPrepareRuntimeImagesInlineStoredAsset(t *testing.T) {
	t.Parallel()

	resolver := useRunconfig(wireComponents(&Service{
		logger: slog.Default(),
	}), func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(_ context.Context, botID, contentHash string) (io.ReadCloser, string, error) {
				if botID != "bot-1" || contentHash != "asset-1" {
					t.Fatalf("unexpected asset lookup: bot=%q hash=%q", botID, contentHash)
				}
				return io.NopCloser(bytes.NewReader(agenttest.RasterPNG(t))), "image/png", nil
			},
		}
	})
	prepared, err := resolver.prepareRuntimeAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "image",
			ContentHash: "asset-1",
			Name:        "screenshot.png",
		}},
	})
	if err != nil {
		t.Fatalf("prepareRuntimeAttachments() error = %v", err)
	}
	images := prepared.Images
	if len(images) != 1 {
		t.Fatalf("prepareRuntimeAttachments().Images = %#v, want one image", images)
	}
	if !bytes.Equal(images[0].Data, agenttest.RasterPNG(t)) || images[0].MimeType != "image/png" {
		t.Fatalf("prepared image = %#v, want inline PNG", images[0])
	}
}

func TestPrepareACPAttachments_UsesFileAndReplyReferences(t *testing.T) {
	t.Parallel()

	resolver := useRunconfig(wireComponents(&Service{
		logger: slog.Default(),
	}), func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			AccessPathFn: func(_ context.Context, _, contentHash string) (string, error) {
				if contentHash != "asset-pdf" {
					t.Fatalf("unexpected content hash: %s", contentHash)
				}
				return "/data/.memoh/media/aa/asset.pdf", nil
			},
		}
	})
	prepared, err := resolver.prepareRuntimeAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "file",
			Name:        "spec.pdf",
			Mime:        "application/pdf",
			ContentHash: "asset-pdf",
		}},
		ReplyAttachments: []runreq.ChatAttachment{{
			Type: "image",
			Name: "old.png",
			URL:  "https://example.com/old.png",
		}},
	})
	if err != nil {
		t.Fatalf("prepareRuntimeAttachments() error = %v", err)
	}
	if len(prepared.Images) != 0 || len(prepared.Context) != 2 || len(prepared.References) != 2 {
		t.Fatalf("prepared attachments = %#v, want two file references", prepared)
	}
	if prepared.Context[0].Path != "/data/.memoh/media/aa/asset.pdf" || prepared.Context[1].URL != "https://example.com/old.png" {
		t.Fatalf("context attachments = %#v, want PDF path and reply URL", prepared.Context)
	}
}

func TestPrepareACPAttachments_PreservesLongPasteFile(t *testing.T) {
	t.Parallel()

	resolver := useRunconfig(wireComponents(&Service{
		logger: slog.Default(),
	}), func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			AccessPathFn: func(_ context.Context, _, contentHash string) (string, error) {
				if contentHash != "pasted-text-hash" {
					t.Fatalf("unexpected content hash: %s", contentHash)
				}
				return "/data/.memoh/media/aa/pasted-text.txt", nil
			},
		}
	})
	prepared, err := resolver.prepareRuntimeAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "file",
			Name:        "pasted-text.txt",
			Mime:        "text/plain",
			ContentHash: "pasted-text-hash",
		}},
	})
	if err != nil {
		t.Fatalf("prepareRuntimeAttachments() error = %v", err)
	}
	if len(prepared.References) != 1 || prepared.Context[0].Path != "/data/.memoh/media/aa/pasted-text.txt" {
		t.Fatalf("prepared attachments = %#v, want pasted text path", prepared)
	}
}

func TestPrepareACPAttachments_FallsBackWhenStoredImageCannotInline(t *testing.T) {
	t.Parallel()

	resolver := useRunconfig(wireComponents(&Service{
		logger: slog.Default(),
	}), func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
				return nil, "", errors.New("asset too large")
			},
			AccessPathFn: func(context.Context, string, string) (string, error) {
				return "/data/.memoh/media/aa/large.png", nil
			},
		}
	})
	prepared, err := resolver.prepareRuntimeAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "image",
			Name:        "large.png",
			ContentHash: "asset-image",
		}},
	})
	if err != nil {
		t.Fatalf("prepareRuntimeAttachments() error = %v", err)
	}
	if len(prepared.Images) != 0 || len(prepared.References) != 1 || prepared.Context[0].Path != "/data/.memoh/media/aa/large.png" {
		t.Fatalf("prepared attachments = %#v, want image file fallback", prepared)
	}
}

func TestPrepareACPAttachments_RejectsInvalidOrUnreachableData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resolver *Service
		input    runreq.ChatAttachment
		wantCode apperror.Code
	}{
		{
			name:     "invalid image base64",
			resolver: wireComponents(&Service{logger: slog.Default()}),
			input: runreq.ChatAttachment{
				Type:   "image",
				Name:   "broken.png",
				Base64: "data:image/png;base64,not-valid***",
			},
			wantCode: apperror.CodeACPAttachmentInvalid,
		},
		{
			name: "stored file without reachable path",
			resolver: useRunconfig(wireComponents(&Service{
				logger: slog.Default(),
			}), func(d *runconfig.Deps) {
				d.Assets = &agenttest.AssetLoader{
					AccessPathFn: func(context.Context, string, string) (string, error) {
						return "", errors.New("missing")
					},
				}
			}),
			input: runreq.ChatAttachment{
				Type:        "file",
				Name:        "missing.pdf",
				ContentHash: "missing",
			},
			wantCode: apperror.CodeACPAttachmentUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.resolver.prepareRuntimeAttachments(context.Background(), runreq.ChatRequest{
				BotID:       "bot-1",
				Attachments: []runreq.ChatAttachment{tt.input},
			})
			problem, _ := server.ProblemFrom(err, "")
			if apperror.CodeOf(err) != tt.wantCode || problem.Status != 400 {
				t.Fatalf("error = %#v, want code %q with status 400", err, tt.wantCode)
			}
		})
	}
}

func TestOutboundAssetRefsToMessageRefs(t *testing.T) {
	t.Parallel()
	refs := []runreq.OutboundAssetRef{
		{ContentHash: "a1", Role: "attachment", Ordinal: 0},
		{ContentHash: "", Role: "attachment", Ordinal: 1},
		{ContentHash: "a2", Ordinal: 2},
	}
	result := runreq.OutboundAssetRefsToMessageRefs(refs)
	if len(result) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(result))
	}
	if result[0].ContentHash != "a1" || result[0].Role != "attachment" {
		t.Fatalf("unexpected ref[0]: %+v", result[0])
	}
	if result[1].ContentHash != "a2" || result[1].Role != "attachment" {
		t.Fatalf("unexpected ref[1]: %+v", result[1])
	}
}

func TestOutboundAssetRefsToMessageRefs_Empty(t *testing.T) {
	t.Parallel()
	result := runreq.OutboundAssetRefsToMessageRefs(nil)
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestSanitizeMessagesNormalizesUserMultipartImageBytes(t *testing.T) {
	t.Parallel()
	content, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "> quoted reply\n\nWhere is Antelope Canyon?"},
		{"type": "image", "image": map[string]any{"0": 137, "1": 80}, "mediaType": "image/png"},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	cleaned := runreq.SanitizeMessages([]runreq.ModelMessage{{
		Role:    "user",
		Content: content,
	}})
	if len(cleaned) != 1 {
		t.Fatalf("expected 1 message, got %d", len(cleaned))
	}
	if bytes.Equal(cleaned[0].Content, content) {
		t.Fatalf("expected user multipart content to be normalized")
	}
	var parts []map[string]any
	if err := json.Unmarshal(cleaned[0].Content, &parts); err != nil {
		t.Fatalf("unmarshal normalized content: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts after normalization, got %d", len(parts))
	}
	if got := parts[0]["text"]; got != "> quoted reply\n\nWhere is Antelope Canyon?" {
		t.Fatalf("unexpected text part: %#v", got)
	}
	image, _ := parts[1]["image"].(string)
	if !strings.HasPrefix(image, "data:image/png;base64,") {
		t.Fatalf("expected data URL image payload, got %#v", parts[1]["image"])
	}
}

func TestSanitizeMessagesKeepsAssistantMultipartMessages(t *testing.T) {
	t.Parallel()
	content, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "answer"},
		{"type": "image", "image": "data:image/png;base64,aGVsbG8="},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	cleaned := runreq.SanitizeMessages([]runreq.ModelMessage{{
		Role:    "assistant",
		Content: content,
	}})
	if len(cleaned) != 1 {
		t.Fatalf("expected 1 message, got %d", len(cleaned))
	}
	if !bytes.Equal(cleaned[0].Content, content) {
		t.Fatalf("assistant multipart content should remain unchanged")
	}
}

func TestNormalizeImagePartsToDataURL_ConvertsIndexedObject(t *testing.T) {
	msg := runreq.ModelMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"text","text":"hello"},
			{"type":"image","image":{"0":82,"1":73,"2":70,"3":70},"mediaType":"image/webp"}
		]`),
	}

	normalized, changed := runreq.NormalizeImagePartsToDataURL(msg)
	if !changed {
		t.Fatal("expected message to be normalized")
	}

	var parts []map[string]any
	if err := json.Unmarshal(normalized.Content, &parts); err != nil {
		t.Fatalf("failed to unmarshal normalized content: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	image, ok := parts[1]["image"].(string)
	if !ok {
		t.Fatalf("expected image to be string data url, got %T", parts[1]["image"])
	}
	expected := "data:image/webp;base64," + base64.StdEncoding.EncodeToString([]byte{82, 73, 70, 70})
	if image != expected {
		t.Fatalf("unexpected data url, got %q", image)
	}
}

func TestNormalizeImagePartsToDataURL_LeavesStringImageUntouched(t *testing.T) {
	original := `[
		{"type":"image","image":"data:image/png;base64,AAAA","mediaType":"image/png"}
	]`
	msg := runreq.ModelMessage{
		Role:    "user",
		Content: json.RawMessage(original),
	}

	normalized, changed := runreq.NormalizeImagePartsToDataURL(msg)
	if changed {
		t.Fatal("expected no normalization for string image")
	}
	if string(normalized.Content) != original {
		t.Fatalf("expected content unchanged, got %s", string(normalized.Content))
	}
}
