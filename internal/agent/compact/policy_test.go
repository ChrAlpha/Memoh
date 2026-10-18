package compact

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
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
			name:          "target override changes the configured target",
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

			if got := AutoThreshold(tc.threshold, tc.budget); got != tc.wantTrigger {
				t.Fatalf("AutoThreshold(%d, %d) = %d, want %d", tc.threshold, tc.budget, got, tc.wantTrigger)
			}
			if got := targetTokens(tc.targetPercent, tc.budget); got != tc.wantTarget {
				t.Fatalf("targetTokens(%v, %d) = %d, want %d", tc.targetPercent, tc.budget, got, tc.wantTarget)
			}
			if got := syncShouldRun(tc.pressure, tc.budget); got != tc.wantSync {
				t.Fatalf("syncShouldRun(%d, %d) = %t, want %t", tc.pressure, tc.budget, got, tc.wantSync)
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

			if got := AutoThreshold(tc.threshold, tc.budget); got != tc.want {
				t.Fatalf("AutoThreshold(%d, %d) = %d, want %d", tc.threshold, tc.budget, got, tc.want)
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

// ListCompactionArtifactLineageBySession reports no summaries: the run-end
// fixed-token measure reads the session the way a turn does.
func (*controllerQueries) ListCompactionArtifactLineageBySession(context.Context, pgtype.UUID) ([]sqlc.BotHistoryMessageCompact, error) {
	return nil, nil
}

type recordingCompactionRunner struct {
	configs []compaction.TriggerConfig
}

func (r *recordingCompactionRunner) RunCompactionSync(_ context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	r.configs = append(r.configs, cfg)
	return compaction.Result{Status: compaction.StatusOK}, nil
}

// fixture is a Compactor over the dependencies a test replaces and rebuilds.
type fixture struct {
	*Compactor
	logger   *slog.Logger
	queries  dbstore.Queries
	messages messagepkg.Service
	models   *models.Service
	settings Settings
	engine   Runner
	sessions runreq.SessionService
}

// rewire rebuilds the Compactor from the fixture's dependencies, as New does
// for the ones a test replaced.
func (f *fixture) rewire() {
	if f.messages == nil {
		f.messages = &agenttest.RecordingMessages{}
	}
	if f.sessions == nil {
		f.sessions = &agenttest.Sessions{}
	}
	f.Compactor = New(Deps{
		Logger: f.logger, Settings: f.settings, Models: f.models, Queries: f.queries, Runner: f.engine,
		Transcript: transcript.New(transcript.Deps{Logger: f.logger, Messages: f.messages, Queries: f.queries, Sessions: f.sessions}),
	})
}

func newControllerPolicyService(t *testing.T, targetPercent *int) (*fixture, *recordingCompactionRunner) {
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
			CompactionTargetPercent: target,
			CompactionModelID:       compactionConfigUUID(t, modelUUID),
		},
	}
	runner := &recordingCompactionRunner{}
	logger := slog.New(slog.DiscardHandler)
	service := &fixture{
		logger:   logger,
		models:   models.NewService(logger, queries),
		queries:  queries,
		settings: settings.NewService(logger, queries, nil, nil),
		engine:   runner,
	}
	service.rewire()
	return service, runner
}

func TestAutomaticCompactionPathsApplyTargetAndBoundAsyncDrain(t *testing.T) {
	t.Parallel()

	service, runner := newControllerPolicyService(t, targetPercentPointer(55))
	req := runreq.ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000453",
		ThreadID: "00000000-0000-0000-0000-000000000454",
	}
	resolved := runreq.ResolvedContext{
		ContextTokenBudget: 200000,
		Pressure:           &runreq.RunPressure{Conversation: 150000},
	}

	runAutomaticCompaction(t, service, req, resolved, 150000)
	if len(runner.configs) != 3 {
		t.Fatalf("async compaction passes = %d, want 3", len(runner.configs))
	}
	for pass, cfg := range runner.configs {
		if cfg.TargetTokens != 110000 {
			t.Fatalf("async pass %d TargetTokens = %d, want 110000", pass+1, cfg.TargetTokens)
		}
		if !cfg.AllowFrontierFusion {
			t.Fatalf("async pass %d AllowFrontierFusion = false, want true", pass+1)
		}
		if cfg.ContextWindowTokens != 200000 {
			t.Fatalf("async pass %d ContextWindowTokens = %d, want 200000", pass+1, cfg.ContextWindowTokens)
		}
	}

	runner.configs = nil
	result := service.runSync(context.Background(), req, 150000, 0, 200000, "")
	if result.Status != compaction.StatusOK {
		t.Fatalf("sync compaction status = %q, want %q", result.Status, compaction.StatusOK)
	}
	if len(runner.configs) != 1 {
		t.Fatalf("sync compaction passes = %d, want 1", len(runner.configs))
	}
	if got := runner.configs[0].TargetTokens; got != 100000 {
		t.Fatalf("sync TargetTokens = %d, want 100000 soft-share cap", got)
	}
	if runner.configs[0].AllowFrontierFusion {
		t.Fatal("sync AllowFrontierFusion = true, want false")
	}
	if got := runner.configs[0].ContextWindowTokens; got != 200000 {
		t.Fatalf("sync ContextWindowTokens = %d, want 200000", got)
	}
}

func TestSyncBackstopTargetCapsAtSoftShare(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		targetPercent *int
		wantAsync     int
		wantSync      int
	}{
		{
			name:          "target above hard backstop",
			targetPercent: targetPercentPointer(90),
			wantAsync:     7200,
			wantSync:      4000,
		},
		{
			name:      "default target remains below cap",
			wantAsync: 3200,
			wantSync:  3200,
		},
		{
			name:          "target below soft share is honored",
			targetPercent: targetPercentPointer(49),
			wantAsync:     3920,
			wantSync:      3920,
		},
		{
			name:          "target at soft share boundary is honored",
			targetPercent: targetPercentPointer(50),
			wantAsync:     4000,
			wantSync:      4000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			service, runner := newControllerPolicyService(t, tc.targetPercent)
			req := runreq.ChatRequest{
				BotID:    "00000000-0000-0000-0000-000000000453",
				ThreadID: "00000000-0000-0000-0000-000000000454",
			}
			resolved := runreq.ResolvedContext{
				ContextTokenBudget: 8000,
				Pressure:           &runreq.RunPressure{Conversation: 4000},
			}

			runAutomaticCompaction(t, service, req, resolved, 4000)
			if len(runner.configs) != maxAsyncPasses {
				t.Fatalf("async compaction passes = %d, want %d", len(runner.configs), maxAsyncPasses)
			}
			for pass, cfg := range runner.configs {
				if cfg.TargetTokens != tc.wantAsync {
					t.Fatalf("async pass %d TargetTokens = %d, want %d", pass+1, cfg.TargetTokens, tc.wantAsync)
				}
			}

			runner.configs = nil
			result := service.runSync(context.Background(), req, 6000, 0, 8000, "")
			if result.Status != compaction.StatusOK {
				t.Fatalf("sync compaction status = %q, want %q", result.Status, compaction.StatusOK)
			}
			if len(runner.configs) != 1 {
				t.Fatalf("sync compaction passes = %d, want 1", len(runner.configs))
			}
			if got := runner.configs[0].TargetTokens; got != tc.wantSync {
				t.Fatalf("sync TargetTokens = %d, want %d", got, tc.wantSync)
			}
		})
	}
}

// Without an engine nothing is compacted: the automatic trigger stands down
// and the synchronous backstop reports a no-op.
func TestWithoutRunnerNothingIsCompacted(t *testing.T) {
	t.Parallel()

	c := New(Deps{Logger: slog.New(slog.DiscardHandler)})
	req := runreq.ChatRequest{BotID: "bot-1", ThreadID: "session-1"}
	if _, ok := c.planAsync(context.Background(), req, runreq.ResolvedContext{ContextTokenBudget: 8000}, 8000); ok {
		t.Fatal("planAsync() = true without a runner")
	}
	res := c.runSync(context.Background(), req, 9000, 0, 8000, "")
	if res.Status != compaction.StatusNoop || res.Reason != compaction.ReasonNoModel {
		t.Fatalf("runSync() = %+v, want a no-op without a model", res)
	}
}

func targetPercentPointer(value int) *int {
	return &value
}

// runAutomaticCompaction runs what maybeCompact starts as a unit, on the
// test's goroutine.
func runAutomaticCompaction(t *testing.T, service *fixture, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int) {
	t.Helper()
	plan, ok := service.planAsync(context.Background(), req, rc, inputTokens)
	if !ok {
		t.Fatal("planAsync() = false, want a compaction to run")
	}
	if err := service.runAsync(context.Background(), plan); err != nil {
		t.Fatalf("runAsync() error = %v", err)
	}
}
