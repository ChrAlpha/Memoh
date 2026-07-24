package timeline

import (
	"errors"
	"fmt"
)

const maxJSONSafeEventCursor int64 = 1<<53 - 1

// DiscussCursorPosition tracks consumed discuss progress in both the durable
// event-cursor domain and the legacy source-timestamp domain. Segments are
// compared inside their own domain, so cursor magnitudes never race the wall
// clock and cursor-less ingest degrades to source-time coverage.
type DiscussCursorPosition struct {
	EventCursor  int64
	SourceCursor int64
}

// Covers reports whether the position already consumed the segment.
func (p DiscussCursorPosition) Covers(seg RenderedSegment) bool {
	if seg.LastEventCursor > 0 {
		return seg.LastEventCursor <= p.EventCursor
	}
	return seg.ReceivedAtMs <= p.SourceCursor
}

// Merge returns the component-wise maximum of both positions.
func (p DiscussCursorPosition) Merge(other DiscussCursorPosition) DiscussCursorPosition {
	if other.EventCursor > p.EventCursor {
		p.EventCursor = other.EventCursor
	}
	if other.SourceCursor > p.SourceCursor {
		p.SourceCursor = other.SourceCursor
	}
	return p
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

// HasUncoveredExternalEvent reports whether any non-self segment lies past the
// consumed position.
func HasUncoveredExternalEvent(rc RenderedContext, position DiscussCursorPosition) bool {
	for _, seg := range rc {
		if seg.IsMyself || seg.IsSelfSent {
			continue
		}
		if !position.Covers(seg) {
			return true
		}
	}
	return false
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
