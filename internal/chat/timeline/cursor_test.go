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
	if _, err := assignEventCursor(MessageEvent{}, maxJSONSafeEventCursor+1); err == nil {
		t.Fatal("expected error for cursor above the JSON-safe range")
	}
}

func TestPushEventThreadsCursorIntoSegments(t *testing.T) {
	pipeline := NewPipeline(RenderParams{})
	rc := pipeline.PushEvent("s1", MessageEvent{
		SessionID:    "s1",
		MessageID:    "m1",
		EventCursor:  7001,
		ReceivedAtMs: 1000,
		TimestampSec: 1,
		Content:      []ContentNode{{Type: "text", Text: "hello"}},
		Conversation: ConversationMeta{Channel: "telegram", ConversationType: "group"},
	})
	if len(rc) != 1 || rc[0].LastEventCursor != 7001 {
		t.Fatalf("expected segment cursor 7001, got %+v", rc)
	}

	rc = pipeline.PushEvent("s1", EditEvent{
		SessionID:    "s1",
		MessageID:    "m1",
		EventCursor:  7002,
		ReceivedAtMs: 2000,
		TimestampSec: 2,
		Content:      []ContentNode{{Type: "text", Text: "hello edited"}},
	})
	if len(rc) != 1 || rc[0].LastEventCursor != 7002 {
		t.Fatalf("expected edit to bump segment cursor to 7002, got %+v", rc)
	}
}

func TestLatestExternalEventCursorGates(t *testing.T) {
	withCursor := textSegment("m1", 1000, "external")
	withCursor.LastEventCursor = 7001
	selfSent := textSegment("m2", 2000, "bot echo")
	selfSent.LastEventCursor = 7002
	selfSent.IsSelfSent = true
	myself := textSegment("m3", 3000, "bot own")
	myself.LastEventCursor = 7003
	myself.IsMyself = true
	legacy := textSegment("m0", 500, "pre-migration")
	rc := RenderedContext{legacy, withCursor, selfSent, myself}

	if got := LatestExternalEventCursor(rc, 0); got != 7001 {
		t.Fatalf("LatestExternalEventCursor = %d, want 7001", got)
	}
	if got := LatestExternalEventCursor(rc, 7001); got != 0 {
		t.Fatalf("expected no external events past 7001, got %d", got)
	}
	if got := LatestExternalEventCursor(RenderedContext{legacy}, 400); got != 500 {
		t.Fatalf("legacy segment must gate by receivedAtMs, got %d", got)
	}
}

func TestHistoryDiscussCursorMapsSourceBoundary(t *testing.T) {
	consumed := textSegment("m1", 1000, "consumed")
	consumed.LastEventCursor = 7001
	alsoConsumed := textSegment("m2", 1500, "consumed too")
	alsoConsumed.LastEventCursor = 7002
	pending := textSegment("m3", 3000, "pending")
	pending.LastEventCursor = 7003
	rc := RenderedContext{consumed, alsoConsumed, pending}

	position := HistoryDiscussCursor(rc, 2000)
	if position.EventCursor != 7002 || position.SourceCursor != 1500 {
		t.Fatalf("position = %+v, want cursor 7002 source 1500", position)
	}
	if got := HistoryDiscussCursor(rc, 0); got != (DiscussCursorPosition{}) {
		t.Fatalf("zero boundary must map to zero position, got %+v", got)
	}
}

func TestConsumedDiscussCursorTakesLatestGate(t *testing.T) {
	legacy := textSegment("m0", 900, "legacy")
	current := textSegment("m1", 1000, "current")
	current.LastEventCursor = 7001
	rc := RenderedContext{legacy, current}

	position := ConsumedDiscussCursor(rc)
	if position.EventCursor != 7001 || position.SourceCursor != 1000 {
		t.Fatalf("position = %+v, want cursor 7001 source 1000", position)
	}
}
