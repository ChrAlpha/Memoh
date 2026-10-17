package transcript

import (
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// AbsoluteMaxTokens resolves the budget for models without a window.
func (t *Reader) AbsoluteMaxTokens() int {
	if t.fallbackTokens > 0 {
		return t.fallbackTokens
	}
	return contextfrag.DefaultContextFallbackTokens
}
