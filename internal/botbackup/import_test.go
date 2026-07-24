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
	if got := restoredEventCursor([]byte(`{"event_cursor":9007199254740990}`)); got != 0 {
		t.Fatalf("cursor near sequence MAXVALUE must be rejected, got %d", got)
	}
	if got := restoredEventCursor([]byte(`{"event_cursor":-5}`)); got != 0 {
		t.Fatalf("negative cursor must be rejected, got %d", got)
	}
}

func TestRestoredConsumedEventCursor(t *testing.T) {
	if got := restoredConsumedEventCursor(424242); got != 424242 {
		t.Fatalf("valid consumed cursor = %d, want 424242", got)
	}
	if got := restoredConsumedEventCursor(9007199254740990); got != 0 {
		t.Fatalf("poisoned consumed cursor must be dropped, got %d", got)
	}
	if got := restoredConsumedEventCursor(-1); got != 0 {
		t.Fatalf("negative consumed cursor must be dropped, got %d", got)
	}
}
