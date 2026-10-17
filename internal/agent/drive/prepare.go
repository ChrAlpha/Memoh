package drive

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
	attachmentpkg "github.com/felinics/memoh/internal/attachment"
	"github.com/felinics/memoh/internal/media"
)

// OutputMediaStore ingests the files an agent's events point at, so the event
// a subscriber receives names an asset it can fetch instead of a path only the
// workspace knows.
type OutputMediaStore interface {
	IngestContainerFile(ctx context.Context, botID, path string) (media.Asset, error)
	Ingest(ctx context.Context, in media.IngestInput) (media.Asset, error)
}

// OutputSpeech synthesizes the audio a speech event asks for. The composition
// root adapts the audio service and the bot's configured speech model onto it.
type OutputSpeech interface {
	SpeechModelID(ctx context.Context, botID string) (string, error)
	Synthesize(ctx context.Context, modelID, text string) (audio []byte, contentType string, err error)
}

// prepareRunEvent rewrites one event on its way from the runtime to the
// subscribers. It runs for every entry, so an attachment or a spoken reply
// looks the same whether the run started from Web, an IM message or a
// schedule.
//
// Three shapes are possible: the event unchanged, the event rewritten with
// ingested attachments, and a speech event replaced by one voice-attachment
// event per synthesized speech. A speech event with no TTS model configured
// yields nothing and is dropped.
func (d *Driver) prepareRunEvent(ctx context.Context, botID string, assets *outboundAssets, ev native.StreamEvent) []native.StreamEvent {
	switch ev.Type {
	case event.Attachment:
		rewritten := d.ingestEventAttachments(ctx, botID, assets, ev)
		return []native.StreamEvent{rewritten}
	case event.Speech:
		return d.synthesizeEventSpeech(ctx, botID, assets, ev)
	default:
		return []native.StreamEvent{ev}
	}
}

// ingestEventAttachments persists what an attachment event points at and
// rewrites the event with the resulting asset references.
func (d *Driver) ingestEventAttachments(ctx context.Context, botID string, assets *outboundAssets, ev native.StreamEvent) native.StreamEvent {
	if len(ev.Attachments) == 0 {
		return ev
	}
	changed := false
	attachments := make([]event.FileAttachment, len(ev.Attachments))
	copy(attachments, ev.Attachments)
	refs := make([]turn.OutboundAssetRef, 0, len(attachments))
	for i, att := range attachments {
		bundle := attachmentBundle(att)
		if strings.TrimSpace(bundle.ContentHash) != "" {
			refs = append(refs, attachmentAssetRef(bundle, ev.ToolCallID, i))
			continue
		}
		if bundle.Path == "" && bundle.Base64 == "" {
			continue
		}
		ingested, ok := d.ingestAttachment(ctx, botID, bundle)
		if !ok {
			continue
		}
		attachments[i] = fileAttachmentFromBundle(ingested)
		refs = append(refs, attachmentAssetRef(ingested, ev.ToolCallID, i))
		changed = true
	}
	assets.add(refs)
	if !changed {
		return ev
	}
	ev.Attachments = attachments
	return ev
}

// ingestAttachment persists one attachment's bytes, from a container path or
// an inline payload. An attachment that cannot be stored keeps the form it
// arrived in: the subscriber shows what it can and the model already saw it.
func (d *Driver) ingestAttachment(ctx context.Context, botID string, bundle attachmentpkg.Bundle) (attachmentpkg.Bundle, bool) {
	if d.outputMedia == nil {
		return attachmentpkg.Bundle{}, false
	}
	bundle = bundle.Normalize()
	if bundle.Path != "" {
		asset, err := d.outputMedia.IngestContainerFile(ctx, botID, bundle.Path)
		if err != nil {
			d.logger.WarnContext(ctx, "ingest output attachment from workspace failed",
				slog.String("bot_id", botID), slog.String("path", bundle.Path), slog.Any("error", err))
			return attachmentpkg.Bundle{}, false
		}
		return bundle.WithAsset(botID, asset), true
	}
	if bundle.Base64 == "" {
		return attachmentpkg.Bundle{}, false
	}
	mimeType := bundle.Mime
	if mimeType == "" {
		mimeType = attachmentpkg.MimeFromDataURL(bundle.Base64)
	}
	decoded, err := attachmentpkg.DecodeBase64(bundle.Base64, media.MaxAssetBytes)
	if err != nil {
		d.logger.WarnContext(ctx, "decode output attachment failed", slog.Any("error", err))
		return attachmentpkg.Bundle{}, false
	}
	asset, err := d.outputMedia.Ingest(ctx, media.IngestInput{
		BotID:    botID,
		Mime:     mimeType,
		Reader:   decoded,
		MaxBytes: media.MaxAssetBytes,
	})
	if err != nil {
		d.logger.WarnContext(ctx, "ingest output attachment failed", slog.Any("error", err))
		return attachmentpkg.Bundle{}, false
	}
	return bundle.WithAsset(botID, asset), true
}

// synthesizeEventSpeech turns a speech event into the voice-attachment events
// its audio produces.
func (d *Driver) synthesizeEventSpeech(ctx context.Context, botID string, assets *outboundAssets, ev native.StreamEvent) []native.StreamEvent {
	if d.outputSpeech == nil || len(ev.Speeches) == 0 {
		return nil
	}
	modelID, err := d.outputSpeech.SpeechModelID(ctx, botID)
	if err != nil || strings.TrimSpace(modelID) == "" {
		d.logger.WarnContext(ctx, "speech event dropped: bot has no TTS model configured", slog.String("bot_id", botID))
		return nil
	}
	prepared := make([]native.StreamEvent, 0, len(ev.Speeches))
	for _, speech := range ev.Speeches {
		text := strings.TrimSpace(speech.Text)
		if text == "" {
			continue
		}
		audio, contentType, synthErr := d.outputSpeech.Synthesize(ctx, modelID, text)
		if synthErr != nil {
			d.logger.WarnContext(ctx, "synthesize speech failed",
				slog.String("bot_id", botID), slog.Any("error", synthErr))
			continue
		}
		bundle := d.voiceAttachment(ctx, botID, contentType, audio)
		assets.add([]turn.OutboundAssetRef{attachmentAssetRef(bundle, ev.ToolCallID, 0)})
		prepared = append(prepared, native.StreamEvent{
			Type:        event.Attachment,
			ToolCallID:  ev.ToolCallID,
			ShortID:     ev.ShortID,
			Attachments: []event.FileAttachment{fileAttachmentFromBundle(bundle)},
		})
	}
	return prepared
}

// voiceAttachment stores synthesized audio and returns the attachment that
// names it, falling back to an inline payload when the media store is absent
// or refuses the write.
func (d *Driver) voiceAttachment(ctx context.Context, botID, contentType string, audio []byte) attachmentpkg.Bundle {
	bundle := attachmentpkg.Bundle{Type: "voice", Mime: contentType, Size: int64(len(audio))}
	if d.outputMedia != nil {
		asset, err := d.outputMedia.Ingest(ctx, media.IngestInput{
			BotID:    botID,
			Mime:     attachmentpkg.NormalizeMime(contentType),
			Reader:   bytes.NewReader(audio),
			MaxBytes: media.MaxAssetBytes,
		})
		if err == nil {
			return bundle.WithAsset(botID, asset)
		}
		d.logger.WarnContext(ctx, "ingest synthesized speech failed", slog.Any("error", err))
	}
	bundle.Base64 = "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(audio)
	return bundle.Normalize()
}

// outboundAssets collects the assets one run's output points at, from the
// preparation step and from whatever the delivery end reports back. References
// are de-duplicated by content hash, tool call and ordinal, so linking an
// asset twice does not depend on LinkAssets being idempotent.
type outboundAssets struct {
	mu   sync.Mutex
	refs []turn.OutboundAssetRef
	seen map[string]bool
}

func newOutboundAssets() *outboundAssets {
	return &outboundAssets{seen: make(map[string]bool)}
}

// add records references, skipping ones already recorded.
func (c *outboundAssets) add(refs []turn.OutboundAssetRef) {
	if c == nil || len(refs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ref := range refs {
		if strings.TrimSpace(ref.ContentHash) == "" {
			continue
		}
		key := ref.ContentHash + "\x00" + ref.Role + "\x00" + strconv.Itoa(ref.Ordinal)
		if c.seen[key] {
			continue
		}
		c.seen[key] = true
		c.refs = append(c.refs, ref)
	}
}

// collected is the collector a ChatRequest carries for the run.
func (c *outboundAssets) collected() []turn.OutboundAssetRef {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]turn.OutboundAssetRef, len(c.refs))
	copy(out, c.refs)
	return out
}

// attachmentBundle converts the wire attachment shape into the shared bundle
// the media ingestion path works with.
func attachmentBundle(att event.FileAttachment) attachmentpkg.Bundle {
	return attachmentpkg.Bundle{
		Type:        att.Type,
		Base64:      att.Base64,
		Path:        att.Path,
		URL:         att.URL,
		PlatformKey: att.PlatformKey,
		Mime:        att.Mime,
		Name:        att.Name,
		ContentHash: att.ContentHash,
		Size:        att.Size,
		Metadata:    att.Metadata,
	}.Normalize()
}

// fileAttachmentFromBundle converts an ingested bundle back into the wire
// shape, carrying the content hash the subscriber resolves.
func fileAttachmentFromBundle(bundle attachmentpkg.Bundle) event.FileAttachment {
	return event.FileAttachment{
		Type:        bundle.Type,
		Base64:      bundle.Base64,
		Path:        bundle.Path,
		URL:         bundle.URL,
		PlatformKey: bundle.PlatformKey,
		Mime:        bundle.Mime,
		Name:        bundle.Name,
		ContentHash: bundle.ContentHash,
		Size:        bundle.Size,
		Metadata:    bundle.Metadata,
	}
}

// attachmentAssetRef is the reference a persisted message links for one
// attachment of an event.
func attachmentAssetRef(bundle attachmentpkg.Bundle, toolCallID string, ordinal int) turn.OutboundAssetRef {
	ref := turn.OutboundAssetRef{
		ContentHash: strings.TrimSpace(bundle.ContentHash),
		Role:        "attachment",
		Ordinal:     ordinal,
		Name:        strings.TrimSpace(bundle.Name),
		Mime:        strings.TrimSpace(bundle.Mime),
		SizeBytes:   bundle.Size,
		Metadata:    bundle.Metadata,
	}
	if ref.Name == "" && bundle.Metadata != nil {
		ref.Name, _ = bundle.Metadata["name"].(string)
	}
	if toolCallID != "" {
		metadata := make(map[string]any, len(bundle.Metadata)+1)
		for k, v := range bundle.Metadata {
			metadata[k] = v
		}
		metadata["tool_call_id"] = toolCallID
		ref.Metadata = metadata
	}
	ref.StorageKey = attachmentpkg.MetadataString(bundle.Metadata, attachmentpkg.MetadataKeyStorageKey)
	return ref
}
