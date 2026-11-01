package discuss

import (
	"context"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// HistoryReader is the lightweight history surface needed by Discuss. Keeping
// it separate from the general message service prevents loading UI/audit data.
type HistoryReader interface {
	ListTurnResponseSourcesWithinBytes(context.Context, string, messagepkg.HistoryPosition, int64) (messagepkg.HistoryLoad, error)
}

// DiscussCheckpointSource reads a session's active history checkpoint for
// composing its rendered context. *timeline.CheckpointSource implements it.
type DiscussCheckpointSource interface {
	Load(ctx context.Context, sessionID string, rc timeline.RenderedContext) (timeline.HistoryCheckpoint, error)
}

type discussHistoryReader struct {
	messages HistoryReader
	// maxBytes selects the newest content window (CM-ADM-001). The query
	// retains the crossing row and only projects the interrupted flag from
	// metadata, so legacy lifecycle audits stay outside the loaded window.
	maxBytes int64
}

// discussHistoryWindow reports what a load admitted against its content
// budget, for admission observability (CM-OBS-001).
type discussHistoryWindow struct {
	Loaded   int
	Bytes    int64
	MaxBytes int64
	// Truncated reports that the budget, not the checkpoint, ended the
	// window: older turn responses after the checkpoint stay in the
	// database.
	Truncated bool
}

// Load reads the byte-budgeted recent window of persisted assistant/tool
// responses after the session's history checkpoint for timeline
// composition. Older responses beyond the budget stay in the database and
// are never measured; the checkpoint stands for the ones before it.
func (r discussHistoryReader) Load(ctx context.Context, sessionID string, after messagepkg.HistoryPosition) ([]timeline.TurnResponseEntry, discussHistoryWindow, error) {
	window := discussHistoryWindow{MaxBytes: r.maxBytes}
	if r.messages == nil {
		return nil, window, nil
	}
	load, err := r.messages.ListTurnResponseSourcesWithinBytes(ctx, sessionID, after, r.maxBytes)
	if err != nil {
		return nil, window, err
	}
	window.Loaded = len(load.Messages)
	window.Truncated = load.Truncated
	for _, message := range load.Messages {
		window.Bytes += int64(len(message.Content))
	}
	return timeline.DecodeTurnResponseEntries(load.Messages), window, nil
}
