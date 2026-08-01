package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/db/postgres/sqlc"
)

func TestShouldFuseFrontierUsesPriorReferenceAndChatShareCaps(t *testing.T) {
	t.Parallel()

	artifactWithTokens := func(id string, tokens int) Artifact {
		artifact := testArtifact(id)
		artifact.Summary = strings.Repeat(id[:1], tokens*4)
		return artifact
	}
	equalDefaultCap := []Artifact{artifactWithTokens("a", 48), artifactWithTokens("b", 48)}
	overDefaultCap := []Artifact{artifactWithTokens("a", 49), artifactWithTokens("b", 48)}
	overChatCapOnly := []Artifact{artifactWithTokens("a", 23), artifactWithTokens("b", 24)}

	tests := []struct {
		name      string
		cfg       TriggerConfig
		artifacts []Artifact
		want      bool
	}{
		{
			name:      "disabled above cap",
			cfg:       TriggerConfig{},
			artifacts: overDefaultCap,
		},
		{
			name:      "one artifact above cap",
			cfg:       TriggerConfig{AllowFrontierFusion: true},
			artifacts: []Artifact{artifactWithTokens("a", 120)},
		},
		{
			name:      "equal to prior reference cap",
			cfg:       TriggerConfig{AllowFrontierFusion: true},
			artifacts: equalDefaultCap,
		},
		{
			name:      "above prior reference cap",
			cfg:       TriggerConfig{AllowFrontierFusion: true},
			artifacts: overDefaultCap,
			want:      true,
		},
		{
			name: "chat share cap is tighter",
			cfg: TriggerConfig{
				AllowFrontierFusion: true,
				ContextWindowTokens: 500,
			},
			artifacts: overChatCapOnly,
			want:      true,
		},
		{
			name:      "unknown chat window keeps prior reference cap",
			cfg:       TriggerConfig{AllowFrontierFusion: true},
			artifacts: overChatCapOnly,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldFuseFrontier(tt.cfg, tt.artifacts, 400); got != tt.want {
				t.Fatalf("shouldFuseFrontier() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRollupArtifactLevelUsesHighestParentLevel(t *testing.T) {
	t.Parallel()

	parents := []Artifact{{Level: 1}, {Level: 4}, {Level: 2}}
	if got := rollupArtifactLevel(parents); got != 5 {
		t.Fatalf("rollupArtifactLevel() = %d, want 5", got)
	}
}

func TestAllocateAbsorbBudgetsIsProportionalAndFloored(t *testing.T) {
	t.Parallel()

	parents := []Artifact{
		{Summary: strings.Repeat("a", 80)},
		{Summary: strings.Repeat("b", 320)},
	}
	budgets, err := allocateAbsorbBudgets(parents, 100)
	if err != nil {
		t.Fatalf("allocateAbsorbBudgets: %v", err)
	}
	if len(budgets) != 2 || budgets[0]+budgets[1] != 100 {
		t.Fatalf("budgets = %v, want two slices totaling 100", budgets)
	}
	if budgets[0] != 20 || budgets[1] != 80 {
		t.Fatalf("budgets = %v, want exact 20:80 proportional slices", budgets)
	}
	markerFloor := absorbedSegmentOverheadTokens(absorbedSourceEarlierSummary) + estimateBytesAsTokens(truncationMarker)
	if budgets[0] < markerFloor || budgets[1] < markerFloor {
		t.Fatalf("budgets = %v, want every slice at least marker floor %d", budgets, markerFloor)
	}

	floored, err := allocateAbsorbBudgets([]Artifact{
		{Summary: strings.Repeat("a", 4)},
		{Summary: strings.Repeat("b", 36)},
	}, 100)
	if err != nil {
		t.Fatalf("allocate floor-constrained budgets: %v", err)
	}
	if floored[0] != markerFloor || floored[1] != 100-markerFloor {
		t.Fatalf("floor-constrained budgets = %v, want [%d %d]", floored, markerFloor, 100-markerFloor)
	}

	multi, err := allocateAbsorbBudgets([]Artifact{
		{Summary: strings.Repeat("a", 48)},
		{Summary: strings.Repeat("b", 56)},
		{Summary: strings.Repeat("c", 296)},
	}, 100)
	if err != nil {
		t.Fatalf("allocate multi-parent budgets: %v", err)
	}
	if multi[0] != markerFloor || multi[1] != 14 || multi[2] != 73 {
		t.Fatalf("multi-parent budgets = %v, want [%d 14 73]", multi, markerFloor)
	}
}

func TestBuildAbsorbedContextUsesRawOrSummaryPerParentBudget(t *testing.T) {
	t.Parallel()

	rawRow := func(t *testing.T, content string) sqlc.ListUncompactedMessagesBySessionRow {
		t.Helper()
		return mkRow(t, "user", jsonStr(content), 100)
	}
	loadErr := errors.New("covered rows unavailable")

	tests := []struct {
		name        string
		artifact    Artifact
		budget      int
		load        absorbedRowsLoader
		wantSource  absorbedSegmentSource
		wantText    string
		wantMarker  bool
		wantMaxCost int
	}{
		{
			name:     "raw transcript fits",
			artifact: Artifact{ID: "raw", Summary: "fallback summary"},
			budget:   100,
			load: func(context.Context, Artifact) ([]sqlc.ListUncompactedMessagesBySessionRow, error) {
				return []sqlc.ListUncompactedMessagesBySessionRow{rawRow(t, "canonical raw transcript")}, nil
			},
			wantSource:  absorbedSourceRawTranscript,
			wantText:    "canonical raw transcript",
			wantMaxCost: 100,
		},
		{
			name:     "raw transcript overflows",
			artifact: Artifact{ID: "overflow", Summary: "short fallback summary"},
			budget:   20,
			load: func(context.Context, Artifact) ([]sqlc.ListUncompactedMessagesBySessionRow, error) {
				return []sqlc.ListUncompactedMessagesBySessionRow{rawRow(t, strings.Repeat("raw transcript ", 100))}, nil
			},
			wantSource:  absorbedSourceEarlierSummary,
			wantText:    "short fallback summary",
			wantMaxCost: 20,
		},
		{
			name:     "oversized summary is truncated",
			artifact: Artifact{ID: "truncate", Summary: strings.Repeat("summary ", 100)},
			budget:   20,
			load: func(context.Context, Artifact) ([]sqlc.ListUncompactedMessagesBySessionRow, error) {
				return []sqlc.ListUncompactedMessagesBySessionRow{rawRow(t, strings.Repeat("raw transcript ", 100))}, nil
			},
			wantSource:  absorbedSourceEarlierSummary,
			wantMarker:  true,
			wantMaxCost: 20,
		},
		{
			name:     "row load failure falls back",
			artifact: Artifact{ID: "load-error", Summary: "summary after load failure"},
			budget:   30,
			load: func(context.Context, Artifact) ([]sqlc.ListUncompactedMessagesBySessionRow, error) {
				return nil, loadErr
			},
			wantSource:  absorbedSourceEarlierSummary,
			wantText:    "summary after load failure",
			wantMaxCost: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			segments, cost, err := buildAbsorbedContext(context.Background(), []Artifact{tt.artifact}, tt.budget, tt.load)
			if err != nil {
				t.Fatalf("buildAbsorbedContext: %v", err)
			}
			if len(segments) != 1 || segments[0].Source != tt.wantSource {
				t.Fatalf("segments = %#v, want one %q segment", segments, tt.wantSource)
			}
			if tt.wantText != "" && !strings.Contains(segments[0].Content, tt.wantText) {
				t.Fatalf("segment content = %q, want containing %q", segments[0].Content, tt.wantText)
			}
			if tt.wantMarker && !strings.Contains(segments[0].Content, truncationMarker) {
				t.Fatalf("segment content = %q, want truncation marker", segments[0].Content)
			}
			if cost > tt.wantMaxCost {
				t.Fatalf("absorb cost = %d, want <= %d", cost, tt.wantMaxCost)
			}
		})
	}
}

func TestBuildAbsorbedContextFallsBackWhenRawRowsDoNotCoverRollup(t *testing.T) {
	t.Parallel()

	directRow := mkRow(t, "user", `"direct delta only"`, 100)
	parent := Artifact{
		ID:      "rollup",
		Summary: "summary carrying inherited context",
		Level:   1,
		Coverage: []CoveredSource{
			strictTestCoveredSource("ancestor-row", 1),
			strictTestCoveredSource(formatUUID(directRow.ID), 2),
		},
	}
	segments, _, err := buildAbsorbedContext(
		context.Background(),
		[]Artifact{parent},
		100,
		func(context.Context, Artifact) ([]sqlc.ListUncompactedMessagesBySessionRow, error) {
			return []sqlc.ListUncompactedMessagesBySessionRow{directRow}, nil
		},
	)
	if err != nil {
		t.Fatalf("buildAbsorbedContext: %v", err)
	}
	if len(segments) != 1 || segments[0].Source != absorbedSourceEarlierSummary {
		t.Fatalf("segments = %#v, want inherited rollup to fall back to its summary", segments)
	}
	if !strings.Contains(segments[0].Content, parent.Summary) {
		t.Fatalf("segment content = %q, want inherited summary %q", segments[0].Content, parent.Summary)
	}
}
