package compaction

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/db/postgres/sqlc"
)

func artifactWithFrontierTokens(t *testing.T, id string, tokens int) Artifact {
	t.Helper()
	if tokens < priorSeparatorTokens {
		t.Fatalf("frontier token fixture %q = %d, want at least %d", id, tokens, priorSeparatorTokens)
	}
	artifact := testArtifact(id)
	artifact.Summary = strings.Repeat(id[:1], (tokens-priorSeparatorTokens)*4)
	if got := frontierSummaryTokens([]Artifact{artifact}); got != tokens {
		t.Fatalf("frontier token fixture %q = %d, want %d", id, got, tokens)
	}
	return artifact
}

func TestFrontierRetainBudgetSubtractsFixedRenderOverhead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		cfg              TriggerConfig
		maxCompactTokens int
		want             int
	}{
		{name: "five thousand", cfg: TriggerConfig{ContextWindowTokens: 5000}, want: 1976},
		{name: "sixteen thousand", cfg: TriggerConfig{ContextWindowTokens: 16000}, want: 8576},
		{name: "sixty four thousand", cfg: TriggerConfig{ContextWindowTokens: 64000}, want: 37376},
		{name: "known window floor", cfg: TriggerConfig{ContextWindowTokens: 1000}, want: 512},
		{name: "unknown window fallback", maxCompactTokens: 400, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := frontierRetainBudget(tt.cfg, tt.maxCompactTokens); got != tt.want {
				t.Fatalf("frontierRetainBudget() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestShouldFuseFrontierUsesEvictionAlignedRetainBudget(t *testing.T) {
	t.Parallel()

	equalFallbackBudget := []Artifact{artifactWithFrontierTokens(t, "a", 50), artifactWithFrontierTokens(t, "b", 50)}
	overFallbackBudget := []Artifact{artifactWithFrontierTokens(t, "a", 51), artifactWithFrontierTokens(t, "b", 50)}
	fiveThousandFrontier := []Artifact{artifactWithFrontierTokens(t, "a", 1300), artifactWithFrontierTokens(t, "b", 1300)}
	sixteenThousandFrontier := []Artifact{
		artifactWithFrontierTokens(t, "a", 900),
		artifactWithFrontierTokens(t, "b", 1100),
		artifactWithFrontierTokens(t, "c", 1250),
		artifactWithFrontierTokens(t, "d", 1400),
		artifactWithFrontierTokens(t, "e", 1600),
		artifactWithFrontierTokens(t, "f", 1850),
	}

	tests := []struct {
		name      string
		cfg       TriggerConfig
		artifacts []Artifact
		want      bool
	}{
		{
			name:      "flag off",
			cfg:       TriggerConfig{ContextWindowTokens: 5000},
			artifacts: fiveThousandFrontier,
		},
		{
			name:      "single artifact",
			cfg:       TriggerConfig{AllowFrontierFusion: true, ContextWindowTokens: 5000},
			artifacts: []Artifact{artifactWithFrontierTokens(t, "a", 2600)},
		},
		{
			name:      "sixteen thousand keeps six level zero artifacts",
			cfg:       TriggerConfig{AllowFrontierFusion: true, ContextWindowTokens: 16000},
			artifacts: sixteenThousandFrontier,
		},
		{
			name:      "five thousand fuses two level zero artifacts",
			cfg:       TriggerConfig{AllowFrontierFusion: true, ContextWindowTokens: 5000},
			artifacts: fiveThousandFrontier,
			want:      true,
		},
		{
			name:      "unknown window equal to fallback budget",
			cfg:       TriggerConfig{AllowFrontierFusion: true},
			artifacts: equalFallbackBudget,
		},
		{
			name:      "unknown window above fallback budget",
			cfg:       TriggerConfig{AllowFrontierFusion: true},
			artifacts: overFallbackBudget,
			want:      true,
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

func TestFusionPostPassFrontierFitsRetainBudgetWithNewestArtifactSlack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		window        int
		artifactSizes []int
		wantFuse      bool
		wantCap       int
		floorBinds    bool
	}{
		{name: "five thousand pair", window: 5000, artifactSizes: []int{1300, 1300}, wantFuse: true, wantCap: 676},
		{name: "five thousand all but newest fallback", window: 5000, artifactSizes: []int{300, 1800}, wantFuse: true, wantCap: 512, floorBinds: true},
		{name: "sixteen thousand benchmark frontier", window: 16000, artifactSizes: []int{900, 1100, 1250, 1400, 1600, 1850}},
		{name: "sixteen thousand overflow", window: 16000, artifactSizes: []int{2400, 2100, 1800, 1500, 1200}, wantFuse: true, wantCap: 1976},
		{name: "sixty four thousand maximum cap", window: 64000, artifactSizes: []int{9500, 8500, 8000, 7500, 7000}, wantFuse: true, wantCap: maxCompactionSummaryTokens},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			artifacts := make([]Artifact, 0, len(tt.artifactSizes))
			for i, size := range tt.artifactSizes {
				artifacts = append(artifacts, artifactWithFrontierTokens(t, string(rune('a'+i)), size))
			}
			cfg := TriggerConfig{AllowFrontierFusion: true, ContextWindowTokens: tt.window}
			retainBudget := frontierRetainBudget(cfg, 0)
			if got := shouldFuseFrontier(cfg, artifacts, 0); got != tt.wantFuse {
				t.Fatalf("shouldFuseFrontier() = %t, want %t (frontier=%d retain=%d)", got, tt.wantFuse, frontierSummaryTokens(artifacts), retainBudget)
			}
			if !tt.wantFuse {
				if tt.window == 16000 && frontierSummaryTokens(artifacts) != 8100 {
					t.Fatalf("16k benchmark frontier = %d, want 8100", frontierSummaryTokens(artifacts))
				}
				return
			}

			absorbed := selectFusionPrefix(artifacts, retainBudget, 512)
			if len(absorbed) == 0 || len(absorbed) >= len(artifacts) {
				t.Fatalf("absorbed artifacts = %d, want a non-empty prefix that preserves the newest", len(absorbed))
			}
			kept := artifacts[len(absorbed):]
			outputCap := frontierFusionOutputCap(retainBudget, kept, maxCompactionSummaryTokens)
			if outputCap != tt.wantCap {
				t.Fatalf("frontierFusionOutputCap() = %d, want %d", outputCap, tt.wantCap)
			}
			postFrontier := append([]Artifact{artifactWithFrontierTokens(t, "fused", outputCap)}, kept...)
			postTokens := frontierSummaryTokens(postFrontier)
			newestSlack := frontierSummaryTokens(artifacts[len(artifacts)-1:])
			if postTokens > retainBudget+newestSlack {
				t.Fatalf("post frontier = %d, exceeds retain %d plus newest slack %d", postTokens, retainBudget, newestSlack)
			}
			if !tt.floorBinds && postTokens > retainBudget {
				t.Fatalf("post frontier = %d, exceeds retain %d without floor fallback", postTokens, retainBudget)
			}
		})
	}
}

func TestFrontierFusionOutputCapDoesNotExceedSummarizerMaximumBelowFloor(t *testing.T) {
	t.Parallel()

	kept := []Artifact{artifactWithFrontierTokens(t, "a", 1800)}
	if got := frontierFusionOutputCap(1976, kept, 400); got != 400 {
		t.Fatalf("frontierFusionOutputCap() = %d, want summarizer maximum 400", got)
	}
}

func TestSelectFusionPrefixAbsorbsOnlyOverflow(t *testing.T) {
	t.Parallel()

	artifactWithTokens := func(id string, tokens int) Artifact {
		artifact := testArtifact(id)
		artifact.Summary = strings.Repeat(id[:1], tokens*4)
		return artifact
	}
	artifacts := []Artifact{
		artifactWithTokens("a", 40),
		artifactWithTokens("b", 30),
		artifactWithTokens("c", 20),
	}

	tests := []struct {
		name         string
		artifacts    []Artifact
		retainBudget int
		fusedReserve int
		wantAbsorbed []string
	}{
		{
			name:         "whole frontier and reserve fit",
			artifacts:    artifacts,
			retainBudget: 120,
			fusedReserve: 20,
		},
		{
			name:         "minimal oldest prefix keeps newest",
			artifacts:    artifacts,
			retainBudget: 100,
			fusedReserve: 20,
			wantAbsorbed: []string{"a"},
		},
		{
			name:         "tiny retain budget absorbs all but newest",
			artifacts:    artifacts,
			retainBudget: 10,
			fusedReserve: 5,
			wantAbsorbed: []string{"a", "b"},
		},
		{
			name: "exactly fitting suffix absorbs nothing extra",
			artifacts: append(append([]Artifact(nil), artifacts...),
				artifactWithTokens("d", 10)),
			retainBudget: 54,
			fusedReserve: 20,
			wantAbsorbed: []string{"a", "b"},
		},
		{
			name: "single artifact prefix is valid",
			artifacts: []Artifact{
				artifactWithTokens("a", 40),
				artifactWithTokens("b", 20),
			},
			retainBudget: 42,
			fusedReserve: 20,
			wantAbsorbed: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			absorbed := selectFusionPrefix(tt.artifacts, tt.retainBudget, tt.fusedReserve)
			if len(absorbed) != len(tt.wantAbsorbed) {
				t.Fatalf("selectFusionPrefix() = %#v, want ids %v", absorbed, tt.wantAbsorbed)
			}
			for i, wantID := range tt.wantAbsorbed {
				if absorbed[i].ID != wantID {
					t.Fatalf("selectFusionPrefix()[%d].ID = %q, want %q", i, absorbed[i].ID, wantID)
				}
			}
			if len(absorbed) >= len(tt.artifacts) {
				t.Fatal("selectFusionPrefix absorbed the newest artifact")
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

func TestShouldFuseFrontierManualFollowsRetainBudget(t *testing.T) {
	belowKnownBudget := []Artifact{artifactWithFrontierTokens(t, "a", 988), artifactWithFrontierTokens(t, "b", 988)}
	overKnownBudget := []Artifact{artifactWithFrontierTokens(t, "a", 989), artifactWithFrontierTokens(t, "b", 988)}
	equalFallbackBudget := []Artifact{artifactWithFrontierTokens(t, "a", 50), artifactWithFrontierTokens(t, "b", 50)}
	overFallbackBudget := []Artifact{artifactWithFrontierTokens(t, "a", 51), artifactWithFrontierTokens(t, "b", 50)}

	known := TriggerConfig{AllowFrontierFusion: true, Manual: true, ContextWindowTokens: 5000}
	if shouldFuseFrontier(known, belowKnownBudget, 400) {
		t.Fatal("manual pass fused below the chat-window retain budget")
	}
	if !shouldFuseFrontier(known, overKnownBudget, 400) {
		t.Fatal("manual pass did not fuse above the chat-window retain budget")
	}

	unknown := TriggerConfig{AllowFrontierFusion: true, Manual: true}
	if shouldFuseFrontier(unknown, equalFallbackBudget, 400) {
		t.Fatal("manual pass fused at the unknown-window fallback boundary")
	}
	if !shouldFuseFrontier(unknown, overFallbackBudget, 400) {
		t.Fatal("manual pass did not fuse above the unknown-window fallback budget")
	}
}
