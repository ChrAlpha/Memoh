package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

// Compaction is always on, so the retired compaction_enabled field is neither
// accepted nor returned; a client that still sends it is ignored, matching
// how the API treats any unknown field.
func TestRetiredCompactionEnabledFieldIsIgnoredAndNotReturned(t *testing.T) {
	var req UpsertRequest
	if err := json.Unmarshal([]byte(`{"compaction_enabled":false,"compaction_threshold":5000}`), &req); err != nil {
		t.Fatalf("legacy field must not be rejected: %v", err)
	}
	if req.CompactionThreshold == nil || *req.CompactionThreshold != 5000 {
		t.Fatalf("other fields must still bind, got %+v", req.CompactionThreshold)
	}
	out, err := json.Marshal(Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "compaction_enabled") {
		t.Fatalf("response still carries compaction_enabled: %s", out)
	}
}
