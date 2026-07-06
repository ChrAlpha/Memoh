package agent

import "testing"

// TestClampStableMessageCount guards the bound used to slice the raw
// (pre-decoration) message array for the prefix-cache comparator's hash
// basis. It replaces the old systemPrepended-shift semantics of
// renderedStableMessageCount: since the comparator now hashes messages
// before any vendor-specific decoration (e.g. Anthropic's system->message
// promotion), there is no leading message to account for here — clamping is
// the only concern.
func TestClampStableMessageCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		count int
		total int
		want  int
	}{
		{"negative clamps to zero", -1, 5, 0},
		{"zero passes through", 0, 5, 0},
		{"in range passes through", 2, 5, 2},
		{"equal to total passes through", 5, 5, 5},
		{"over range clamps to total", 9, 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clampStableMessageCount(tc.count, tc.total); got != tc.want {
				t.Fatalf("clampStableMessageCount(%d, %d) = %d, want %d", tc.count, tc.total, got, tc.want)
			}
		})
	}
}
