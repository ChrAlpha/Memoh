package runconfig

import (
	"bytes"
	"context"
	"io"
	"testing"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/models"
)

func visionModel() models.GetResponse {
	return models.GetResponse{Model: models.Model{Config: models.ModelConfig{Compatibilities: []string{models.CompatVision}}}}
}

func batchRecord(id, mime, hash string) historyfrag.HistoryRecord {
	return historyfrag.HistoryRecord{DBMessageID: id, Assets: []historyfrag.MediaRef{{ContentHash: hash, Mime: mime}}}
}

// Only a batch row still in the run's context has its images read: a row a
// summary covers is no longer among the records, and a file is not vision
// input. An animated sticker is, whatever label it is stored under.
func TestDiscussBatchImagesReadsOnlyRowsInTheContext(t *testing.T) {
	var opened []string
	data := agenttest.RasterPNG(t)
	b := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(_ context.Context, _, hash string) (io.ReadCloser, string, error) {
				opened = append(opened, hash)
				return io.NopCloser(bytes.NewReader(data)), "image/png", nil
			},
		}
	})
	req := runreq.ChatRequest{
		BotID: "bot-1", SessionType: sessionmode.Discuss, RequiredHistoryMessageID: "current",
		DiscussBatchMessageIDs: []string{"covered", "older", "document", "current"},
	}
	records := []historyfrag.HistoryRecord{
		batchRecord("older", "image/png", "older-hash"),
		batchRecord("document", "application/pdf", "pdf-hash"),
		batchRecord("answered", "image/png", "answered-hash"),
		batchRecord("current", "image/jpeg", "current-hash"),
	}

	got := b.discussBatchImages(context.Background(), visionModel(), req, records)

	if len(opened) != 2 || opened[0] != "current-hash" || opened[1] != "older-hash" {
		t.Fatalf("opened %v, want the current row's picture and the older row's, newest first", opened)
	}
	if len(got.current) != 1 || len(got.byMessage["older"]) != 1 || len(got.byMessage) != 1 || len(got.omitted) != 0 {
		t.Fatalf("discussBatchImages() = %+v, want one picture on the current input and one on the older row", got)
	}
}

func TestDiscussBatchImagesTakesAnAnimatedStickerByItsBytes(t *testing.T) {
	data := agenttest.MovingStickerTGS(t)
	b := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
				return io.NopCloser(bytes.NewReader(data)), "application/x-tgsticker", nil
			},
		}
	})
	req := runreq.ChatRequest{
		BotID: "bot-1", SessionType: sessionmode.Discuss, RequiredHistoryMessageID: "current",
		DiscussBatchMessageIDs: []string{"sticker"},
	}

	got := b.discussBatchImages(context.Background(), visionModel(), req,
		[]historyfrag.HistoryRecord{batchRecord("sticker", "application/x-tgsticker", "tgs")})

	if frames := got.byMessage["sticker"]; len(frames) != AnimationFrameCount {
		t.Fatalf("sticker contributes %d images, want its %d frames", len(frames), AnimationFrameCount)
	}
}

// Every other session type keeps what it had: only a discuss run's batch is
// read back.
func TestDiscussBatchImagesLeavesOtherSessionsAlone(t *testing.T) {
	opens := 0
	b := newBuilder(func(d *Deps) {
		d.Assets = &agenttest.AssetLoader{
			OpenFn: func(context.Context, string, string) (io.ReadCloser, string, error) {
				opens++
				return nil, "", io.EOF
			},
		}
	})
	req := runreq.ChatRequest{BotID: "bot-1", SessionType: sessionmode.Chat, DiscussBatchMessageIDs: []string{"m1"}}
	got := b.discussBatchImages(context.Background(), visionModel(), req,
		[]historyfrag.HistoryRecord{batchRecord("m1", "image/png", "h")})
	if opens != 0 || got.current != nil || got.byMessage != nil {
		t.Fatalf("opened %d, got %+v; want nothing for a chat session", opens, got)
	}
}
