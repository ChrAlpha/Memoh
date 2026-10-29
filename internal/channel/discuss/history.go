package discuss

import (
	"context"
	"time"

	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/chat/timeline"
)

// HistoryReader is the lightweight history surface needed by Discuss. Keeping
// it separate from the general message service prevents loading UI/audit data.
type HistoryReader interface {
	ListTurnResponseSourcesSinceBySessionWithinBytes(context.Context, string, time.Time, int64) ([]messagepkg.Message, error)
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
}

// BudgetReached reports whether the walk stopped at the budget, leaving any
// older responses in the database.
func (w discussHistoryWindow) BudgetReached() bool {
	return w.MaxBytes > 0 && w.Bytes >= w.MaxBytes
}

// Load reads the byte-budgeted recent window of persisted assistant/tool
// responses for timeline composition. Older responses beyond the budget stay
// in the database; compaction artifacts represent them in composition.
func (r discussHistoryReader) Load(ctx context.Context, sessionID string) ([]timeline.TurnResponseEntry, discussHistoryWindow, error) {
	window := discussHistoryWindow{MaxBytes: r.maxBytes}
	if r.messages == nil {
		return nil, window, nil
	}
	messages, err := r.messages.ListTurnResponseSourcesSinceBySessionWithinBytes(ctx, sessionID, time.Unix(0, 0).UTC(), r.maxBytes)
	if err != nil {
		return nil, window, err
	}
	window.Loaded = len(messages)
	for _, message := range messages {
		window.Bytes += int64(len(message.Content))
	}
	return timeline.DecodeTurnResponseEntries(messages), window, nil
}
