package botbackup

import (
	"encoding/json"
	"testing"
)

func TestSanitizeRestoredEventData(t *testing.T) {
	stripped := sanitizeRestoredEventData([]byte(`{"event_cursor":424242,"message_id":"m1","received_at_ms":1000}`))
	var payload map[string]any
	if err := json.Unmarshal(stripped, &payload); err != nil {
		t.Fatalf("decode sanitized payload: %v", err)
	}
	if _, ok := payload["event_cursor"]; ok {
		t.Fatal("instance-local cursor must be stripped from restored payloads")
	}
	if payload["message_id"] != "m1" || payload["received_at_ms"] != float64(1000) {
		t.Fatalf("other fields must survive, got %v", payload)
	}
}

func TestSanitizeRestoredEventDataPassthrough(t *testing.T) {
	plain := []byte(`{"message_id":"m1"}`)
	if got := string(sanitizeRestoredEventData(plain)); got != string(plain) {
		t.Fatalf("payload without cursor must pass through, got %s", got)
	}
	malformed := []byte(`not json`)
	if got := string(sanitizeRestoredEventData(malformed)); got != string(malformed) {
		t.Fatalf("malformed payload must pass through, got %s", got)
	}
}
