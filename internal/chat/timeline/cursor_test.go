package timeline

import (
	"testing"
)

func TestAssignEventCursorStampsAllEventKinds(t *testing.T) {
	events := []CanonicalEvent{
		MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1000},
		EditEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1100},
		DeleteEvent{SessionID: "s", MessageIDs: []string{"m1"}, ReceivedAtMs: 1200},
		ServiceEvent{SessionID: "s", Action: ServiceMemberLeft, ReceivedAtMs: 1300},
	}
	for _, event := range events {
		stamped, err := assignEventCursor(event, 5001)
		if err != nil {
			t.Fatalf("assign cursor to %T: %v", event, err)
		}
		if got := eventCursorOf(stamped); got != 5001 {
			t.Fatalf("%T cursor = %d, want 5001", stamped, got)
		}
	}
}

func eventCursorOf(event CanonicalEvent) int64 {
	switch typed := event.(type) {
	case MessageEvent:
		return typed.EventCursor
	case EditEvent:
		return typed.EventCursor
	case DeleteEvent:
		return typed.EventCursor
	case ServiceEvent:
		return typed.EventCursor
	default:
		return -1
	}
}

func TestAssignEventCursorRejectsInvalid(t *testing.T) {
	if _, err := assignEventCursor(nil, 1); err == nil {
		t.Fatal("expected error for nil event")
	}
	if _, err := assignEventCursor(MessageEvent{}, 0); err == nil {
		t.Fatal("expected error for non-positive cursor")
	}
	if _, err := assignEventCursor(MessageEvent{}, MaxJSONSafeEventCursor+1); err == nil {
		t.Fatal("expected error for cursor above the JSON-safe range")
	}
}
