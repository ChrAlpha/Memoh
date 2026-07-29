package application

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/agent/context/compaction"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/settings"
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

type controllerQueries struct {
	*compactionConfigQueries
	settings sqlc.GetSettingsByBotIDRow
}

func (q *controllerQueries) GetSettingsByBotID(context.Context, pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return q.settings, nil
}

type recordingCompactionRunner struct {
	configs []compaction.TriggerConfig
}

func (r *recordingCompactionRunner) RunCompactionSync(_ context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	r.configs = append(r.configs, cfg)
	return compaction.Result{Status: compaction.StatusOK}, nil
}

func TestAutomaticCompactionPathsShareTargetAndBoundAsyncDrain(t *testing.T) {
	t.Parallel()

	const (
		modelUUID    = "00000000-0000-0000-0000-000000000451"
		providerUUID = "00000000-0000-0000-0000-000000000452"
		botUUID      = "00000000-0000-0000-0000-000000000453"
		threadUUID   = "00000000-0000-0000-0000-000000000454"
	)
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
			Language:                "auto",
			ReasoningEffort:         "medium",
			HeartbeatInterval:       30,
			CompactionEnabled:       true,
			CompactionTargetPercent: pgtype.Int4{Int32: 55, Valid: true},
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
	req := ChatRequest{BotID: botUUID, ThreadID: threadUUID}
	resolved := resolvedContext{
		contextTokenBudget:     200000,
		compactableTokens:      150000,
		compactableTokensKnown: true,
	}

	service.maybeCompact(context.Background(), req, resolved, 150000)
	if len(runner.configs) != 3 {
		t.Fatalf("async compaction passes = %d, want 3", len(runner.configs))
	}
	for pass, cfg := range runner.configs {
		if cfg.TargetTokens != 110000 {
			t.Fatalf("async pass %d TargetTokens = %d, want 110000", pass+1, cfg.TargetTokens)
		}
	}

	runner.configs = nil
	result := service.runCompactionSync(context.Background(), req, 150000, 200000, "")
	if result.Status != compaction.StatusOK {
		t.Fatalf("sync compaction status = %q, want %q", result.Status, compaction.StatusOK)
	}
	if len(runner.configs) != 1 {
		t.Fatalf("sync compaction passes = %d, want 1", len(runner.configs))
	}
	if got := runner.configs[0].TargetTokens; got != 110000 {
		t.Fatalf("sync TargetTokens = %d, want 110000", got)
	}
}

func targetPercentPointer(value int) *int {
	return &value
}
