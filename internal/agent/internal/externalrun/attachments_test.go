package externalrun

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runconfig"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/server"
)

func TestPrepareRuntimeImagesInlineStoredAsset(t *testing.T) {
	t.Parallel()

	resolver := withRunconfig(t, func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(_ context.Context, botID, contentHash string) (io.ReadCloser, string, error) {
				if botID != "bot-1" || contentHash != "asset-1" {
					t.Fatalf("unexpected asset lookup: bot=%q hash=%q", botID, contentHash)
				}
				return io.NopCloser(bytes.NewReader(agenttest.RasterPNG(t))), "image/png", nil
			},
		}
	})
	prepared, err := resolver.prepareAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "image",
			ContentHash: "asset-1",
			Name:        "screenshot.png",
		}},
	})
	if err != nil {
		t.Fatalf("prepareAttachments() error = %v", err)
	}
	images := prepared.Images
	if len(images) != 1 {
		t.Fatalf("prepareAttachments().Images = %#v, want one image", images)
	}
	if !bytes.Equal(images[0].Data, agenttest.RasterPNG(t)) || images[0].MimeType != "image/png" {
		t.Fatalf("prepared image = %#v, want inline PNG", images[0])
	}
}

func TestPrepareACPAttachments_UsesFileAndReplyReferences(t *testing.T) {
	t.Parallel()

	resolver := withRunconfig(t, func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			AccessPathFn: func(_ context.Context, _, contentHash string) (string, error) {
				if contentHash != "asset-pdf" {
					t.Fatalf("unexpected content hash: %s", contentHash)
				}
				return "/data/.memoh/media/aa/asset.pdf", nil
			},
		}
	})
	prepared, err := resolver.prepareAttachments(context.Background(), runreq.ChatRequest{
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
		t.Fatalf("prepareAttachments() error = %v", err)
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

	resolver := withRunconfig(t, func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			AccessPathFn: func(_ context.Context, _, contentHash string) (string, error) {
				if contentHash != "pasted-text-hash" {
					t.Fatalf("unexpected content hash: %s", contentHash)
				}
				return "/data/.memoh/media/aa/pasted-text.txt", nil
			},
		}
	})
	prepared, err := resolver.prepareAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "file",
			Name:        "pasted-text.txt",
			Mime:        "text/plain",
			ContentHash: "pasted-text-hash",
		}},
	})
	if err != nil {
		t.Fatalf("prepareAttachments() error = %v", err)
	}
	if len(prepared.References) != 1 || prepared.Context[0].Path != "/data/.memoh/media/aa/pasted-text.txt" {
		t.Fatalf("prepared attachments = %#v, want pasted text path", prepared)
	}
}

func TestPrepareACPAttachments_FallsBackWhenStoredImageCannotInline(t *testing.T) {
	t.Parallel()

	resolver := withRunconfig(t, func(d *runconfig.Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
				return nil, "", errors.New("asset too large")
			},
			AccessPathFn: func(context.Context, string, string) (string, error) {
				return "/data/.memoh/media/aa/large.png", nil
			},
		}
	})
	prepared, err := resolver.prepareAttachments(context.Background(), runreq.ChatRequest{
		BotID: "bot-1",
		Attachments: []runreq.ChatAttachment{{
			Type:        "image",
			Name:        "large.png",
			ContentHash: "asset-image",
		}},
	})
	if err != nil {
		t.Fatalf("prepareAttachments() error = %v", err)
	}
	if len(prepared.Images) != 0 || len(prepared.References) != 1 || prepared.Context[0].Path != "/data/.memoh/media/aa/large.png" {
		t.Fatalf("prepared attachments = %#v, want image file fallback", prepared)
	}
}

func TestPrepareACPAttachments_RejectsInvalidOrUnreachableData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resolver *Host
		input    runreq.ChatAttachment
		wantCode apperror.Code
	}{
		{
			name:     "invalid image base64",
			resolver: testHost(t, Deps{}),
			input: runreq.ChatAttachment{
				Type:   "image",
				Name:   "broken.png",
				Base64: "data:image/png;base64,not-valid***",
			},
			wantCode: apperror.CodeACPAttachmentInvalid,
		},
		{
			name: "stored file without reachable path",
			resolver: withRunconfig(t, func(d *runconfig.Deps) {
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
			_, err := tt.resolver.prepareAttachments(context.Background(), runreq.ChatRequest{
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
