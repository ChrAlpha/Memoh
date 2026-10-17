package drive

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	attachmentpkg "github.com/felinics/memoh/internal/attachment"
)

// Synthesized speech that the media store cannot keep still reaches the
// subscriber: the voice attachment carries the audio inline instead.
func TestVoiceAttachmentFallsBackToAnInlinePayload(t *testing.T) {
	t.Parallel()

	driver := New(Deps{})
	bundle := driver.voiceAttachment(context.Background(), "bot-1", "audio/mpeg", []byte("audio"))

	if bundle.Type != "voice" {
		t.Fatalf("type = %q, want voice", bundle.Type)
	}
	if !strings.HasPrefix(bundle.Base64, "data:audio/mpeg;base64,") {
		t.Fatalf("base64 = %q, want the inline data URL", bundle.Base64)
	}
	if bundle.URL != "" || bundle.ContentHash != "" {
		t.Fatalf("bundle = %#v, want no stored reference", bundle)
	}
}

// The reference a persisted reply links for an attachment carries what the
// history needs to show it: its name (from metadata when the bundle has none),
// its storage key, its size, and the tool call that produced it.
func TestAttachmentAssetRefCarriesWhatHistoryShows(t *testing.T) {
	t.Parallel()

	bundle := attachmentpkg.Bundle{
		Type:        "image",
		ContentHash: "asset-1",
		Mime:        "image/png",
		Size:        42,
		Metadata: map[string]any{
			"name":                              "demo.png",
			attachmentpkg.MetadataKeyStorageKey: "aa/asset-1.png",
		},
	}
	ref := attachmentAssetRef(bundle, "call-42", 0)

	if ref.ContentHash != "asset-1" || ref.Role != "attachment" {
		t.Fatalf("ref = %#v, want the asset as an attachment", ref)
	}
	if ref.Name != "demo.png" || ref.StorageKey != "aa/asset-1.png" || ref.SizeBytes != 42 {
		t.Fatalf("ref = %#v, want the metadata name, storage key and size", ref)
	}
	if got, _ := ref.Metadata["tool_call_id"].(string); got != "call-42" {
		t.Fatalf("tool_call_id metadata = %q, want call-42", got)
	}
	if _, touched := bundle.Metadata["tool_call_id"]; touched {
		t.Fatal("the reference wrote its tool call into the event's own metadata")
	}
}

// A file the agent's output points at in the workspace is stored, and the
// event the subscriber sees names it by content hash.
func TestPrepareRunEventIngestsAWorkspaceFile(t *testing.T) {
	t.Parallel()

	driver := New(Deps{OutputMedia: agenttest.OutputMedia{}})
	assets := newOutboundAssets()
	prepared := driver.prepareRunEvent(context.Background(), "bot-1", assets, native.StreamEvent{
		Type:        native.EventAttachment,
		Attachments: []native.FileAttachment{{Type: "image", Path: "/data/images/demo.png"}},
	})

	if len(prepared) != 1 || len(prepared[0].Attachments) != 1 {
		t.Fatalf("prepared = %#v, want the one attachment event", prepared)
	}
	att := prepared[0].Attachments[0]
	if att.ContentHash != "hash:/data/images/demo.png" {
		t.Fatalf("content hash = %q, want the stored asset's", att.ContentHash)
	}
	if got := assets.collected(); len(got) != 1 || got[0].ContentHash != att.ContentHash {
		t.Fatalf("collected = %#v, want the stored asset for the reply", got)
	}
}
