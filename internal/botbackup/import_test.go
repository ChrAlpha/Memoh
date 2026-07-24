package botbackup

import "testing"

func TestRestoredEventCursor(t *testing.T) {
	if got := restoredEventCursor([]byte(`{"event_cursor":424242,"message_id":"m1"}`)); got != 424242 {
		t.Fatalf("cursor = %d, want 424242", got)
	}
	if got := restoredEventCursor([]byte(`{"message_id":"m1"}`)); got != 0 {
		t.Fatalf("missing cursor = %d, want 0", got)
	}
	if got := restoredEventCursor([]byte(`not json`)); got != 0 {
		t.Fatalf("malformed payload = %d, want 0", got)
	}
	if got := restoredEventCursor([]byte(`{"event_cursor":9007199254740991}`)); got != 0 {
		t.Fatalf("cursor at sequence MAXVALUE must be rejected, got %d", got)
	}
	if got := restoredEventCursor([]byte(`{"event_cursor":-5}`)); got != 0 {
		t.Fatalf("negative cursor must be rejected, got %d", got)
	}
}
