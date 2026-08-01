package botbackup

import "testing"

func TestDecodeBackupSettingsCompactionRatioCompatibility(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		raw        string
		wantTarget *int
	}{
		{
			name:       "legacy manual ratio maps to keep share",
			raw:        `{"compaction_threshold":100000,"compaction_ratio":80}`,
			wantTarget: backupIntPointer(20),
		},
		{
			name:       "legacy minimum ratio maps to maximum target",
			raw:        `{"compaction_threshold":100000,"compaction_ratio":1}`,
			wantTarget: backupIntPointer(99),
		},
		{
			name: "zero config ignores its legacy dead ratio",
			raw:  `{"compaction_threshold":0,"compaction_ratio":80}`,
		},
		{
			name: "legacy ratio producing invalid target keeps default",
			raw:  `{"compaction_threshold":100000,"compaction_ratio":100}`,
		},
		{
			name:       "new target wins",
			raw:        `{"compaction_threshold":100000,"compaction_ratio":80,"compaction_target_percent":55}`,
			wantTarget: backupIntPointer(55),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := decodeBackupSettings([]byte(tc.raw))
			if err != nil {
				t.Fatalf("decodeBackupSettings() error = %v", err)
			}
			if !equalBackupInt(got.CompactionTargetPercent, tc.wantTarget) {
				t.Fatalf("CompactionTargetPercent = %v, want %v", got.CompactionTargetPercent, tc.wantTarget)
			}
		})
	}
}

func backupIntPointer(value int) *int {
	return &value
}

func equalBackupInt(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
