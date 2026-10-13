package compaction

import "testing"

// The same table as TestPostgresReplaceTurnInvalidationMatchesCutoffAdmission.
// Artifacts it admits are the ones ReplaceHistoryTurn keeps valid.
func TestHistoryCutoffAdmitsOnlyArtifactsEndingBeforeCutoff(t *testing.T) {
	const cutoffMs int64 = 1_000_000
	cutoff := &HistoryCutoff{BeforeMs: cutoffMs}
	tests := []struct {
		name     string
		artifact Artifact
		admitted bool
	}{
		{"ends before the cutoff", Artifact{AnchorStartMs: 1, AnchorEndMs: cutoffMs - 1}, true},
		{"ends at the cutoff", Artifact{AnchorStartMs: 1, AnchorEndMs: cutoffMs}, false},
		{"ends after the cutoff", Artifact{AnchorStartMs: 1, AnchorEndMs: cutoffMs + 5}, false},
		{"no anchor start", Artifact{AnchorStartMs: 0, AnchorEndMs: cutoffMs - 1}, false},
		{"inverted range", Artifact{AnchorStartMs: cutoffMs + 5, AnchorEndMs: cutoffMs - 1}, false},
		{"pending claim", Artifact{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutoff.admitsArtifact(tc.artifact); got != tc.admitted {
				t.Fatalf("admitsArtifact = %v, want %v", got, tc.admitted)
			}
			kept := cutoff.artifactsBeforeCutoff([]Artifact{tc.artifact})
			if (len(kept) == 1) != tc.admitted {
				t.Fatalf("artifactsBeforeCutoff kept %d, want admitted=%v", len(kept), tc.admitted)
			}
		})
	}
}
