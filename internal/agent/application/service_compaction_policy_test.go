package application

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

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
		wantHard      bool
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
			wantHard:    true,
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
			wantHard:    true,
		},
		{
			name:          "target override changes the configured target",
			threshold:     90000,
			targetPercent: targetPercentPointer(55),
			budget:        200000,
			pressure:      150000,
			wantTrigger:   90000,
			wantTarget:    110000,
			wantHard:      true,
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
			wantHard:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := AutoCompactionThreshold(tc.threshold, tc.budget); got != tc.wantTrigger {
				t.Fatalf("AutoCompactionThreshold(%d, %d) = %d, want %d", tc.threshold, tc.budget, got, tc.wantTrigger)
			}
			if got := compactionTargetTokens(tc.targetPercent, tc.budget); got != tc.wantTarget {
				t.Fatalf("compactionTargetTokens(%v, %d) = %d, want %d", tc.targetPercent, tc.budget, got, tc.wantTarget)
			}
			if got := compactionUnderHardPressure(tc.pressure, tc.budget); got != tc.wantHard {
				t.Fatalf("compactionUnderHardPressure(%d, %d) = %t, want %t", tc.pressure, tc.budget, got, tc.wantHard)
			}
		})
	}
}

func TestAutoCompactionThreshold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		threshold int
		budget    int
		want      int
	}{
		{name: "zero budget yields no mark", threshold: 90000},
		{name: "negative budget yields no mark", threshold: 90000, budget: -1},
		{name: "unset threshold uses the soft share", budget: 200000, want: 100000},
		{name: "threshold below hard share is honored", threshold: 90000, budget: 200000, want: 90000},
		{name: "threshold above hard share is capped", threshold: 500000, budget: 200000, want: 150000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := AutoCompactionThreshold(tc.threshold, tc.budget); got != tc.want {
				t.Fatalf("AutoCompactionThreshold(%d, %d) = %d, want %d", tc.threshold, tc.budget, got, tc.want)
			}
		})
	}
}

type controllerQueries struct {
	*compactionConfigQueries
	settings sqlc.GetSettingsByBotIDRow
}

func (q *controllerQueries) GetSettingsByBotID(context.Context, pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return q.settings, nil
}

type recordingCompactionRunner struct {
	configs []compaction.TriggerConfig
	errs    []error // scripted per pass; nil means a committed summary
	results []compaction.Result
}

func (r *recordingCompactionRunner) RunCompactionSync(_ context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	r.configs = append(r.configs, cfg)
	if pass := len(r.configs) - 1; pass < len(r.errs) && r.errs[pass] != nil {
		return compaction.Result{}, r.errs[pass]
	}
	if pass := len(r.configs) - 1; pass < len(r.results) {
		return r.results[pass], nil
	}
	return compaction.Result{Status: compaction.StatusOK}, nil
}

func newControllerPolicyService(t *testing.T, targetPercent *int) (*Service, *recordingCompactionRunner) {
	t.Helper()
	const (
		modelUUID    = "00000000-0000-0000-0000-000000000451"
		providerUUID = "00000000-0000-0000-0000-000000000452"
		botUUID      = "00000000-0000-0000-0000-000000000453"
	)
	target := pgtype.Int4{}
	if targetPercent != nil {
		target = pgtype.Int4{Int32: int32(*targetPercent), Valid: true} //nolint:gosec // test values stay within 1..99
	}
	queries := &controllerQueries{
		compactionConfigQueries: &compactionConfigQueries{
			model: sqlc.Model{
				ID:         compactionConfigUUID(t, modelUUID),
				ModelID:    "compact-model",
				ProviderID: compactionConfigUUID(t, providerUUID),
				Type:       "chat",
				Enable:     true,
				Config:     []byte(`{"context_window":200000}`),
			},
			provider: sqlc.Provider{
				ID:         compactionConfigUUID(t, providerUUID),
				Name:       "test-provider",
				ClientType: "openai-completions",
				Enable:     true,
				Config:     []byte(`{"api_key":"test-key"}`),
			},
		},
		settings: sqlc.GetSettingsByBotIDRow{
			BotID:                   compactionConfigUUID(t, botUUID),
			ReasoningEffort:         "medium",
			CompactionEnabled:       true,
			CompactionTargetPercent: target,
			CompactionModelID:       compactionConfigUUID(t, modelUUID),
		},
	}
	runner := &recordingCompactionRunner{}
	service := &Service{
		logger:            slog.New(slog.DiscardHandler),
		modelsService:     models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:           queries,
		settingsService:   settings.NewService(slog.New(slog.DiscardHandler), queries, nil, nil),
		compactionService: runner,
	}
	return service, runner
}

func TestAutomaticCompactionAppliesTargetAndBoundsDrain(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, targetPercentPointer(55))
	req := ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
	}

	runAutomaticCompaction(t, service, req, 120000, 200000)
	if len(runner.configs) != maxCompactionPasses {
		t.Fatalf("compaction passes = %d, want %d", len(runner.configs), maxCompactionPasses)
	}
	for pass, cfg := range runner.configs {
		if cfg.TargetTokens != 110000 {
			t.Fatalf("pass %d TargetTokens = %d, want 110000", pass+1, cfg.TargetTokens)
		}
		if !cfg.AllowFrontierFusion {
			t.Fatalf("pass %d AllowFrontierFusion = false, want true", pass+1)
		}
		if cfg.ContextWindowTokens != 200000 {
			t.Fatalf("pass %d ContextWindowTokens = %d, want 200000", pass+1, cfg.ContextWindowTokens)
		}
		if cfg.HardPressure {
			t.Fatalf("pass %d HardPressure = true, want false below 75%% of the window", pass+1)
		}
	}

	runner.configs = nil
	result := service.runBudgetCompactionSync(context.Background(), req, 150000, 200000, "")
	if result.Status != compaction.StatusOK {
		t.Fatalf("recovery compaction status = %q, want %q", result.Status, compaction.StatusOK)
	}
	if len(runner.configs) != 1 {
		t.Fatalf("recovery compaction passes = %d, want 1", len(runner.configs))
	}
	if got := runner.configs[0].TargetTokens; got != 100000 {
		t.Fatalf("recovery TargetTokens = %d, want 100000 soft-share cap", got)
	}
	if !runner.configs[0].AllowFrontierFusion {
		t.Fatal("recovery must be able to replace oversized summaries")
	}
	if got := runner.configs[0].HistoryBudgetTokens; got != 200000 {
		t.Fatalf("recovery HistoryBudgetTokens = %d, want 200000", got)
	}
}

// A configured target at or above the hard share would keep every raw token
// once pressure is that high; a hard-pressure pass lands below the soft
// trigger instead.
func TestHardPressureCapsTheTargetAtTheSoftShare(t *testing.T) {
	t.Parallel()

	req := ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
	}
	cases := []struct {
		name          string
		targetPercent *int
		pressure      int
		wantTarget    int
	}{
		{name: "high target under hard pressure", targetPercent: targetPercentPointer(90), pressure: 160000, wantTarget: 100000},
		{name: "high target under soft pressure", targetPercent: targetPercentPointer(90), pressure: 120000, wantTarget: 180000},
		{name: "target at the soft share under hard pressure", targetPercent: targetPercentPointer(50), pressure: 160000, wantTarget: 100000},
		{name: "low target under hard pressure", targetPercent: targetPercentPointer(30), pressure: 160000, wantTarget: 60000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service, runner := newControllerPolicyService(t, tc.targetPercent)
			runAutomaticCompaction(t, service, req, tc.pressure, 200000)
			if len(runner.configs) == 0 {
				t.Fatal("no compaction pass ran")
			}
			if got := runner.configs[0].TargetTokens; got != tc.wantTarget {
				t.Fatalf("TargetTokens = %d, want %d", got, tc.wantTarget)
			}
			if got, want := runner.configs[0].HardPressure, tc.pressure >= 150000; got != want {
				t.Fatalf("HardPressure = %t, want %t", got, want)
			}
		})
	}
}

func TestSetCompactionServicePreservesNil(t *testing.T) {
	t.Parallel()

	service := &Service{}
	service.SetCompactionService(nil)
	if service.compactionService != nil {
		t.Fatal("SetCompactionService(nil) stored a non-nil runner")
	}
}

func targetPercentPointer(value int) *int {
	return &value
}

// runAutomaticCompaction runs what scheduleCompaction starts as a unit, on
// the test's goroutine.
func runAutomaticCompaction(t *testing.T, service *Service, req ChatRequest, pressure, contextTokenBudget int) {
	t.Helper()
	plan, ok := service.planCompaction(context.Background(), req, pressure, contextTokenBudget, "")
	if !ok {
		t.Fatal("planCompaction() = false, want a compaction to run")
	}
	if err := service.runCompaction(context.Background(), plan); err != nil {
		t.Fatalf("runCompaction() error = %v", err)
	}
}

func TestDrainContinuesPastAnIneffectiveSummary(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	runner.errs = []error{fmt.Errorf("pass: %w", compaction.ErrIneffectiveSummary)}
	req := ChatRequest{BotID: "00000000-0000-0000-0000-000000000453", ThreadID: "00000000-0000-0000-0000-000000000454"}
	runAutomaticCompaction(t, service, req, 4000, 8000)
	if len(runner.configs) != maxCompactionPasses {
		t.Fatalf("drain ran %d passes, want %d: the next pass selects past the rows that did not shrink", len(runner.configs), maxCompactionPasses)
	}
}

func TestRecoveryRunsPastAnIneffectiveSummary(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, nil)
	runner.errs = []error{fmt.Errorf("pass: %w", compaction.ErrIneffectiveSummary)}
	req := ChatRequest{BotID: "00000000-0000-0000-0000-000000000453", ThreadID: "00000000-0000-0000-0000-000000000454"}
	result := service.runBudgetCompactionSync(context.Background(), req, 150000, 200000, "")
	if result.Status != compaction.StatusOK || len(runner.configs) != 2 {
		t.Fatalf("recovery = %q after %d passes, want the next pass to move past the rows that did not shrink", result.Status, len(runner.configs))
	}
}

func TestScanContinuationIsBoundedAndDoesNotReportRecovery(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			service, runner := newControllerPolicyService(t, nil)
			for range maxAsyncCompactionPasses {
				runner.results = append(runner.results, compaction.Result{Status: compaction.StatusNoop, ScanPending: pending})
			}
			if err := service.drainCompactionBacklog(t.Context(), compaction.TriggerConfig{}); err != nil {
				t.Fatal(err)
			}
			want := 1
			if pending {
				want = maxAsyncCompactionPasses
			}
			if len(runner.configs) != want {
				t.Fatalf("async passes=%d, want %d", len(runner.configs), want)
			}
			runner.configs = nil
			req := ChatRequest{BotID: "00000000-0000-0000-0000-000000000453", ThreadID: "00000000-0000-0000-0000-000000000454"}
			res := service.runCompactionSync(t.Context(), req, 150000, 200000, "")
			if res.Status != compaction.StatusNoop || len(runner.configs) != min(2, want) {
				t.Fatalf("sync scan reported recovery or exceeded its bound: %+v passes=%d", res, len(runner.configs))
			}
		})
	}
}
