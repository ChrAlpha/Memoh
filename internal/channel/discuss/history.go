package discuss

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// HistoryReader is the lightweight history surface needed by Discuss. Keeping
// it separate from the general message service prevents loading UI/audit data.
type HistoryReader interface {
	MeasureHistory(context.Context, string, messagepkg.HistoryPosition) (messagepkg.ActiveMessagesMeasure, error)
	ListTurnResponseSourcesWithinBytes(context.Context, string, messagepkg.HistoryPosition, int64) ([]messagepkg.Message, error)
}

// DiscussCheckpointSource reads a session's active history checkpoint for
// composing its rendered context, once it has stepped over the history from
// before checkpoints that a composition within limits cannot render.
// *timeline.CheckpointSource implements it.
type DiscussCheckpointSource interface {
	StepOver(ctx context.Context, sessionID string, limits checkpoint.StepOverLimits) (int64, error)
	Load(ctx context.Context, sessionID string, rc timeline.RenderedContext) (timeline.HistoryCheckpoint, error)
}

type discussHistoryReader struct {
	messages HistoryReader
	// maxBytes selects the newest content window (CM-ADM-001). The query
	// retains the crossing row and only projects the interrupted flag from
	// metadata, so legacy lifecycle audits stay outside the loaded window.
	maxBytes int64
	logger   *slog.Logger
}

// discussHistoryMeasure reports the database-side aggregate next to what was
// actually loaded, for admission observability (CM-OBS-001).
type discussHistoryMeasure struct {
	TotalMessages int64
	TotalBytes    int64
	Loaded        int
}

// Load reads the byte-budgeted recent window of persisted assistant/tool
// responses after the session's history checkpoint for timeline
// composition, measuring the full extent with a metadata-only aggregate
// first. Older responses beyond the budget stay in the database; the
// checkpoint stands for the ones before it.
func (r discussHistoryReader) Load(ctx context.Context, sessionID string, after messagepkg.HistoryPosition) ([]timeline.TurnResponseEntry, discussHistoryMeasure, error) {
	if r.messages == nil {
		return nil, discussHistoryMeasure{}, nil
	}
	measure := discussHistoryMeasure{}
	if agg, err := r.messages.MeasureHistory(ctx, sessionID, after); err == nil {
		measure.TotalMessages = agg.MessageCount
		measure.TotalBytes = agg.ContentBytes
	} else {
		r.logger.WarnContext(ctx, "measure TRs failed", slog.String("session_id", sessionID), slog.Any("error", err))
	}
	messages, err := r.messages.ListTurnResponseSourcesWithinBytes(ctx, sessionID, after, r.maxBytes)
	if err != nil {
		return nil, measure, err
	}
	measure.Loaded = len(messages)
	return timeline.DecodeTurnResponseEntries(messages), measure, nil
}
