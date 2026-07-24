package timeline

import (
	"errors"
	"fmt"
)

const maxJSONSafeEventCursor int64 = 1<<53 - 1

// DiscussCursorPosition tracks consumed discuss progress in both the durable
// event-cursor domain and the legacy source-timestamp domain.
type DiscussCursorPosition struct {
	EventCursor  int64
	SourceCursor int64
}

func assignEventCursor(event CanonicalEvent, cursor int64) (CanonicalEvent, error) {
	if event == nil {
		return nil, errors.New("canonical event is nil")
	}
	if cursor <= 0 || cursor > maxJSONSafeEventCursor {
		return nil, fmt.Errorf("event cursor %d is outside the JSON-safe range", cursor)
	}
	switch typed := event.(type) {
	case MessageEvent:
		typed.EventCursor = cursor
		return typed, nil
	case EditEvent:
		typed.EventCursor = cursor
		return typed, nil
	case DeleteEvent:
		typed.EventCursor = cursor
		return typed, nil
	case ServiceEvent:
		typed.EventCursor = cursor
		return typed, nil
	default:
		return nil, fmt.Errorf("unsupported canonical event type %T", event)
	}
}

// GateCursor is the segment's trigger-gate value: the durable event cursor
// when the segment has one, else its source timestamp. The cursor sequence is
// seeded above the wall clock at migration, so legacy timestamps stay below
// every allocated cursor.
func (seg RenderedSegment) GateCursor() int64 {
	if seg.LastEventCursor > 0 {
		return seg.LastEventCursor
	}
	return seg.ReceivedAtMs
}

// LatestExternalEventCursor returns the highest gate value of non-self
// segments past afterCursor, or 0 if none found.
func LatestExternalEventCursor(rc RenderedContext, afterCursor int64) int64 {
	var latest int64
	for _, seg := range rc {
		if seg.IsMyself || seg.IsSelfSent {
			continue
		}
		if gate := seg.GateCursor(); gate > afterCursor && gate > latest {
			latest = gate
		}
	}
	return latest
}

// HistoryDiscussCursor maps a legacy source-timestamp boundary onto the
// rendered timeline, seeding a cursor-domain position from the segments the
// boundary already covers.
func HistoryDiscussCursor(rc RenderedContext, sourceBoundary int64) DiscussCursorPosition {
	position := DiscussCursorPosition{}
	if sourceBoundary <= 0 {
		return position
	}
	for _, seg := range rc {
		if seg.ReceivedAtMs > sourceBoundary {
			continue
		}
		if seg.LastEventCursor > position.EventCursor {
			position.EventCursor = seg.LastEventCursor
		}
		if seg.ReceivedAtMs > position.SourceCursor {
			position.SourceCursor = seg.ReceivedAtMs
		}
	}
	return position
}

// ConsumedDiscussCursor is the position a discuss turn consumes when it
// replies to the given rendered timeline.
func ConsumedDiscussCursor(rc RenderedContext) DiscussCursorPosition {
	position := DiscussCursorPosition{}
	for _, seg := range rc {
		if seg.LastEventCursor > position.EventCursor {
			position.EventCursor = seg.LastEventCursor
		}
		if seg.ReceivedAtMs > position.SourceCursor {
			position.SourceCursor = seg.ReceivedAtMs
		}
	}
	return position
}
