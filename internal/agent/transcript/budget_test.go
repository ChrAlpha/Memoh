package transcript

import (
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func TestAbsoluteMaxTokens(t *testing.T) {
	t.Parallel()

	if got := New(Deps{}).AbsoluteMaxTokens(); got != contextfrag.DefaultContextFallbackTokens {
		t.Fatalf("default budget = %d, want %d", got, contextfrag.DefaultContextFallbackTokens)
	}
	if got := New(Deps{FallbackTokens: 1234}).AbsoluteMaxTokens(); got != 1234 {
		t.Fatalf("configured budget = %d, want 1234", got)
	}
}
