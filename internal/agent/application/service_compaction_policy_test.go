package application

import "testing"

func TestUnifiedCompactionController(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		threshold     int
		targetPercent *int
		budget        int
		pressure      int
		wantTrigger   int
		wantTarget    int
		wantSync      bool
	}{
		{
			name:        "zero config uses 50 75 40",
			budget:      200000,
			pressure:    149999,
			wantTrigger: 100000,
			wantTarget:  80000,
		},
		{
			name:        "hard gate fires at 75 percent",
			budget:      200000,
			pressure:    150000,
			wantTrigger: 100000,
			wantTarget:  80000,
			wantSync:    true,
		},
		{
			name:        "absolute threshold only moves async trigger",
			threshold:   90000,
			budget:      200000,
			pressure:    149999,
			wantTrigger: 90000,
			wantTarget:  80000,
		},
		{
			name:        "absolute threshold clamps to hard gate",
			threshold:   500000,
			budget:      200000,
			pressure:    150000,
			wantTrigger: 150000,
			wantTarget:  80000,
			wantSync:    true,
		},
		{
			name:          "target override changes the shared target",
			threshold:     90000,
			targetPercent: targetPercentPointer(55),
			budget:        200000,
			pressure:      150000,
			wantTrigger:   90000,
			wantTarget:    110000,
			wantSync:      true,
		},
		{
			name:          "zero budget stands down",
			threshold:     90000,
			targetPercent: targetPercentPointer(55),
			pressure:      150000,
		},
		{
			name:          "small positive budget keeps controller active",
			threshold:     100,
			targetPercent: targetPercentPointer(1),
			budget:        1,
			pressure:      1,
			wantTrigger:   1,
			wantTarget:    1,
			wantSync:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := autoCompactionThreshold(tc.threshold, tc.budget); got != tc.wantTrigger {
				t.Fatalf("autoCompactionThreshold(%d, %d) = %d, want %d", tc.threshold, tc.budget, got, tc.wantTrigger)
			}
			if got := compactionTargetTokens(tc.targetPercent, tc.budget); got != tc.wantTarget {
				t.Fatalf("compactionTargetTokens(%v, %d) = %d, want %d", tc.targetPercent, tc.budget, got, tc.wantTarget)
			}
			if got := syncCompactionShouldRun(tc.pressure, tc.budget); got != tc.wantSync {
				t.Fatalf("syncCompactionShouldRun(%d, %d) = %t, want %t", tc.pressure, tc.budget, got, tc.wantSync)
			}
		})
	}
}

func TestAsyncCompactionAlwaysUsesBoundedDrain(t *testing.T) {
	t.Parallel()

	if maxAsyncCompactionPasses != 3 {
		t.Fatalf("maxAsyncCompactionPasses = %d, want 3", maxAsyncCompactionPasses)
	}
}

func targetPercentPointer(value int) *int {
	return &value
}
