package timeline

import (
	"errors"
	"fmt"
)

// MaxJSONSafeEventCursor bounds legitimate event cursors; the allocation
// sequence never exceeds it, so larger payload values are corrupt data.
const MaxJSONSafeEventCursor int64 = 1<<53 - 1

func assignEventCursor(event CanonicalEvent, cursor int64) (CanonicalEvent, error) {
	if event == nil {
		return nil, errors.New("canonical event is nil")
	}
	if cursor <= 0 || cursor > MaxJSONSafeEventCursor {
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
