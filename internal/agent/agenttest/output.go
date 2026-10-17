package agenttest

import (
	"context"
	"time"

	"github.com/felinics/memoh/internal/media"
)

// OutputMedia stores every file it is given and names it by its path.
type OutputMedia struct{}

func (OutputMedia) IngestContainerFile(_ context.Context, _ string, path string) (media.Asset, error) {
	return media.Asset{ContentHash: "hash:" + path, Mime: "image/png", StorageKey: "store/" + path}, nil
}

func (OutputMedia) Ingest(context.Context, media.IngestInput) (media.Asset, error) {
	return media.Asset{ContentHash: "hash:voice", Mime: "audio/mpeg", StorageKey: "store/voice"}, nil
}

// OutputSpeech synthesizes a fixed clip after an optional delay.
type OutputSpeech struct {
	Audio       []byte
	ContentType string
	Delay       time.Duration
}

func (*OutputSpeech) SpeechModelID(context.Context, string) (string, error) {
	return "speech-model", nil
}

func (f *OutputSpeech) Synthesize(ctx context.Context, _, _ string) ([]byte, string, error) {
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return f.Audio, f.ContentType, nil
}
