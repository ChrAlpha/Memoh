package persist

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// The text a failed run keeps is the text of the model call that failed: a
// retried call is regenerated from the last committed step, and a step end
// hands the text to that step's commit.
func TestUncommittedStepTextStartsOverAtRetryAndStepEnd(t *testing.T) {
	for _, boundary := range []native.StreamEventType{native.EventRetry, native.EventStepEnd} {
		var text UncommittedStepText
		for _, event := range []native.StreamEvent{
			{Type: native.EventTextDelta, Delta: "before"},
			{Type: boundary},
			{Type: native.EventTextDelta, Delta: "part"},
			{Type: native.EventTextDelta, Delta: "ial"},
		} {
			text.Observe(event)
		}
		if got := text.String(); got != "partial" {
			t.Fatalf("uncommitted text after %s = %q, want %q", boundary, got, "partial")
		}
	}
}
