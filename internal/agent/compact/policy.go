package compact

import "github.com/felinics/memoh/internal/agent/context/compaction"

// Automatic compaction derives its soft trigger, hard backstop, and target
// from the chat model's context window. A configured absolute threshold only
// overrides the async trigger and is capped at the hard backstop.
const (
	softThresholdPercent = 50
	hardThresholdPercent = 75
	defaultTargetPercent = 40
	// maxAsyncPasses bounds how many consecutive summarizer calls
	// one background trigger may spend draining a backlog.
	maxAsyncPasses = 3
)

// AutoThreshold is the async trigger level, exported so read-side
// surfaces label the same level the turn path acts on. A zero return leaves
// automatic compaction off when no usable model window is available. The
// synchronous backstop is deliberately not exposed: a reader cannot observe it.
func AutoThreshold(userThreshold, contextTokenBudget int) int {
	if contextTokenBudget <= 0 {
		return 0
	}
	if userThreshold <= 0 {
		return max(1, contextTokenBudget*softThresholdPercent/100)
	}
	return min(userThreshold, hardThreshold(contextTokenBudget))
}

func hardThreshold(contextTokenBudget int) int {
	if contextTokenBudget <= 0 {
		return 0
	}
	return max(1, contextTokenBudget*hardThresholdPercent/100)
}

func targetTokens(targetPercent *int, contextTokenBudget int) int {
	if contextTokenBudget <= 0 {
		return 0
	}
	percent := defaultTargetPercent
	if targetPercent != nil && *targetPercent >= 1 && *targetPercent <= 99 {
		percent = *targetPercent
	}
	return max(1, contextTokenBudget*percent/100)
}

// syncBackstopTargetTokens caps the hard-share backstop at the soft share so it
// always makes progress and lands below the soft trigger, restoring hysteresis.
func syncBackstopTargetTokens(targetPercent *int, contextTokenBudget int) int {
	softTarget := max(1, contextTokenBudget*softThresholdPercent/100)
	return min(targetTokens(targetPercent, contextTokenBudget), softTarget)
}

func syncShouldRun(pressure, contextTokenBudget int) bool {
	threshold := hardThreshold(contextTokenBudget)
	return threshold > 0 && pressure >= threshold
}

// urgency is how urgently a context needs compacting.
type urgency int

const (
	urgencyNone urgency = iota
	// urgencyAsync starts a background compaction; the turn proceeds.
	urgencyAsync
	// urgencySync blocks the turn until compaction has run.
	urgencySync
)

// urgencyOf is the one place that turns context pressure into a
// compaction decision. userThreshold, when positive, replaces the soft share
// that triggers the background run; it never exceeds the hard share.
func urgencyOf(pressure, contextTokenBudget, userThreshold int) urgency {
	switch {
	case syncShouldRun(pressure, contextTokenBudget):
		return urgencySync
	case compaction.ShouldCompact(pressure, AutoThreshold(userThreshold, contextTokenBudget)):
		return urgencyAsync
	default:
		return urgencyNone
	}
}
