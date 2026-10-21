package transcript

import (
	"testing"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// A run always names its turn, so a handle without one is refused instead of
// answered with an empty request row.
func TestContinuationRequestRefusesAHandleWithoutATurn(t *testing.T) {
	t.Parallel()
	reader := New(Deps{})
	if _, err := reader.ContinuationRequest(t.Context(), "session-1", sessionruntime.RunHandle{RunID: "run-1"}); err == nil {
		t.Fatal("ContinuationRequest() error = nil, want an error for a handle with no turn")
	}
}
