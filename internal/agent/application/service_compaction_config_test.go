package application

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

type compactionConfigQueries struct {
	dbstore.Queries
	model    sqlc.Model
	provider sqlc.Provider
}

func (q *compactionConfigQueries) GetModelByID(context.Context, pgtype.UUID) (sqlc.Model, error) {
	return q.model, nil
}

func (q *compactionConfigQueries) GetProviderByID(context.Context, pgtype.UUID) (sqlc.Provider, error) {
	return q.provider, nil
}

func (*compactionConfigQueries) GetProviderOAuthTokenByProvider(context.Context, pgtype.UUID) (sqlc.ProviderOauthToken, error) {
	return sqlc.ProviderOauthToken{}, pgx.ErrNoRows
}

func (*compactionConfigQueries) GetLatestSessionModelID(context.Context, pgtype.UUID) (pgtype.UUID, error) {
	return pgtype.UUID{}, pgx.ErrNoRows
}

func compactionConfigUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	parsed, err := db.ParseUUID(id)
	if err != nil {
		t.Fatalf("ParseUUID(%q): %v", id, err)
	}
	return parsed
}

func TestBuildCompactionConfigLeavesSelectionToController(t *testing.T) {
	t.Parallel()

	const modelUUID = "00000000-0000-0000-0000-000000000401"
	const providerUUID = "00000000-0000-0000-0000-000000000402"

	queries := &compactionConfigQueries{
		model: sqlc.Model{
			ID:         compactionConfigUUID(t, modelUUID),
			ModelID:    "compact-model",
			ProviderID: compactionConfigUUID(t, providerUUID),
			Type:       "chat",
			Enable:     true,
			Config:     []byte(`{"context_window":200000,"max_output_tokens":32000}`),
		},
		provider: sqlc.Provider{
			ID:         compactionConfigUUID(t, providerUUID),
			Name:       "test-provider",
			ClientType: "openai-completions",
			Enable:     true,
			Config:     []byte(`{"api_key":"test-key"}`),
		},
	}
	r := &Service{
		logger:        slog.New(slog.DiscardHandler),
		modelsService: models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:       queries,
	}

	cfg, err := r.buildCompactionConfig(context.Background(), ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000403",
		ThreadID: "00000000-0000-0000-0000-000000000404",
	}, settings.Settings{
		CompactionModelID:       modelUUID,
		CompactionTargetPercent: intPointer(40),
	}, 150000, "")
	if err != nil {
		t.Fatalf("buildCompactionConfig: %v", err)
	}

	if cfg.TotalInputTokens != 150000 {
		t.Fatalf("TotalInputTokens = %d, want 150000", cfg.TotalInputTokens)
	}
	if cfg.SummaryWindowTokens != 200000 {
		t.Fatalf("SummaryWindowTokens = %d, want the full summarizer window", cfg.SummaryWindowTokens)
	}
	if cfg.ModelRecordID == "" {
		t.Fatal("ModelRecordID must carry the models row UUID for artifact provenance")
	}
	if cfg.MaxOutputTokens != 32000 {
		t.Fatalf("MaxOutputTokens = %d, want the model's declared output cap", cfg.MaxOutputTokens)
	}
}

func TestBuildCompactionConfigSkipsProviderWithoutOutputLimit(t *testing.T) {
	t.Parallel()

	const modelUUID = "00000000-0000-0000-0000-000000000411"
	const providerUUID = "00000000-0000-0000-0000-000000000412"
	queries := &compactionConfigQueries{
		model: sqlc.Model{
			ID:         compactionConfigUUID(t, modelUUID),
			ModelID:    "codex-compact-model",
			ProviderID: compactionConfigUUID(t, providerUUID),
			Type:       string(models.ModelTypeChat),
			Enable:     true,
		},
		provider: sqlc.Provider{
			ID:         compactionConfigUUID(t, providerUUID),
			Name:       "codex-provider",
			ClientType: string(models.ClientTypeOpenAICodex),
			Enable:     true,
		},
	}
	service := &Service{
		logger:        slog.New(slog.DiscardHandler),
		modelsService: models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:       queries,
	}

	cfg, err := service.buildCompactionConfig(context.Background(), ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000413",
		ThreadID: "00000000-0000-0000-0000-000000000414",
	}, settings.Settings{
		CompactionModelID:       modelUUID,
		CompactionTargetPercent: intPointer(20),
	}, 150000, "")
	if !errors.Is(err, models.ErrCompactionOutputLimitUnsupported) || cfg.ModelID != "" {
		t.Fatalf("buildCompactionConfig() = %q, %v; want the unsupported summarizer reported", cfg.ModelID, err)
	}
}

func TestBuildCompactionConfigUsesTurnModelWhenNoOverride(t *testing.T) {
	t.Parallel()

	const modelUUID = "00000000-0000-0000-0000-000000000431"
	const providerUUID = "00000000-0000-0000-0000-000000000432"
	queries := &compactionConfigQueries{
		model: sqlc.Model{
			ID:         compactionConfigUUID(t, modelUUID),
			ModelID:    "turn-selected-model",
			ProviderID: compactionConfigUUID(t, providerUUID),
			Type:       "chat",
			Enable:     true,
			Config:     []byte(`{"context_window":128000}`),
		},
		provider: sqlc.Provider{
			ID:         compactionConfigUUID(t, providerUUID),
			Name:       "test-provider",
			ClientType: "openai-completions",
			Enable:     true,
			Config:     []byte(`{"api_key":"test-key"}`),
		},
	}
	service := &Service{
		logger:        slog.New(slog.DiscardHandler),
		modelsService: models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:       queries,
	}

	cfg, err := service.buildCompactionConfig(context.Background(), ChatRequest{
		BotID:    "00000000-0000-0000-0000-000000000433",
		ThreadID: "00000000-0000-0000-0000-000000000434",
	}, settings.Settings{}, 90000, modelUUID)
	if err != nil {
		t.Fatalf("buildCompactionConfig: %v", err)
	}
	if cfg.ModelRecordID != modelUUID {
		t.Fatalf("ModelRecordID = %q, want the turn's actually-resolved model %q", cfg.ModelRecordID, modelUUID)
	}
	if cfg.ModelID != "turn-selected-model" {
		t.Fatalf("ModelID = %q, want the turn model slug", cfg.ModelID)
	}
}
