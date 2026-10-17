package application

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// imageInputService is a Service whose asset store serves the named assets.
func imageInputService(t *testing.T, assets map[string][]byte) *Service {
	t.Helper()
	return useRunconfig(wireComponents(&Service{logger: slog.Default()}), func(d *runconfig.Deps) {
		d.Assets = agenttest.ImageAssets(t, assets)
	})
}

// An External Agent gets the frames too, and still one reachable original.
func TestAnimatedStickerReachesExternalAgentAsFrames(t *testing.T) {
	s := imageInputService(t, map[string][]byte{"sticker": agenttest.MovingStickerTGS(t)})
	prepared, err := s.prepareRuntimeAttachments(context.Background(), runreq.ChatRequest{
		BotID:       "bot-1",
		Attachments: []runreq.ChatAttachment{{Type: "image", ContentHash: "sticker", Mime: "image/webp", Name: "sticker.tgs"}},
	})
	if err != nil {
		t.Fatalf("prepareRuntimeAttachments() error = %v", err)
	}
	if len(prepared.Images) != runconfig.AnimationFrameCount {
		t.Fatalf("Images = %d, want %d frames", len(prepared.Images), runconfig.AnimationFrameCount)
	}
	for i, image := range prepared.Images {
		if image.MimeType != "image/png" || !bytes.HasPrefix(image.Data, []byte{0x89, 'P', 'N', 'G'}) {
			t.Fatalf("image %d = %q with %d bytes, want PNG", i, image.MimeType, len(image.Data))
		}
	}
	if len(prepared.References) != 1 || len(prepared.Context) != 1 {
		t.Fatalf("references=%v context=%d, want the original counted once", prepared.References, len(prepared.Context))
	}
}
