package native

import (
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestTokenEstimateFromBytesMatchesSharedLedger(t *testing.T) {
	t.Parallel()

	for _, n := range []int{-1, 0, 3, 4, 7, 8, 4096} {
		if got, want := tokenEstimateFromBytes(n), contextfrag.TokensFromBytes(n); got != want {
			t.Fatalf("tokenEstimateFromBytes(%d) = %d, want shared ledger value %d", n, got, want)
		}
	}
}
