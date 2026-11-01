package compaction

import (
	"strings"
	"testing"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// The longest summary the prompt may ask for is one that, with a token to
// spare, replays within the budget as the request carries it, when one a
// token longer does not or would pass the model's limit.
func TestSummaryOutputLimitIsTheLongestSummaryThatReplays(t *testing.T) {
	t.Parallel()
	replays := func(tokens, budget int) bool {
		return summaryReplayTokens(strings.Repeat("x", (tokens+1)*contextfrag.EstimateBytesPerToken)) <= budget
	}
	check := func(budget, limit int) {
		most := max(min(limit, budget), 0)
		got := summaryOutputLimit(budget, limit)
		if got < 0 || got > most || got > 0 && !replays(got, budget) || got < most && replays(got+1, budget) {
			t.Fatalf("summaryOutputLimit(%d, %d) = %d: not the longest summary within %d that replays", budget, limit, got, most)
		}
	}
	for budget := -1; budget <= 1200; budget++ {
		for _, limit := range []int{0, 1, 33, 64000} {
			check(budget, limit)
		}
	}
	for _, budget := range []int{32000, 32001, 32002, 32003, 32004} {
		check(budget, 64000)
	}
}
