package compaction

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/providers"
)

type triggerModelSourceStub struct {
	byID map[string]models.GetResponse
}

func (s triggerModelSourceStub) GetByID(_ context.Context, id string) (models.GetResponse, error) {
	model, ok := s.byID[id]
	if !ok {
		return models.GetResponse{}, errors.New("model not found: " + id)
	}
	return model, nil
}

type triggerProviderQueries struct {
	dbstore.Queries
	provider           sqlc.Provider
	latestSessionModel pgtype.UUID
}

func (q triggerProviderQueries) GetProviderByID(_ context.Context, _ pgtype.UUID) (sqlc.Provider, error) {
	return q.provider, nil
}

func (q triggerProviderQueries) GetLatestSessionModelID(_ context.Context, _ pgtype.UUID) (pgtype.UUID, error) {
	return q.latestSessionModel, nil
}

type triggerCredentialsStub struct{}

func (triggerCredentialsStub) ResolveModelCredentials(_ context.Context, _ sqlc.Provider) (providers.ModelCredentials, error) {
	return providers.ModelCredentials{APIKey: "resolved-key", CodexAccountID: "resolved-codex"}, nil
}

func triggerTestModel(window int, enabled bool) models.GetResponse {
	response := models.GetResponse{}
	response.ID = "5f6f0e9c-8d31-4a51-9f6e-2f42f1f3b7aa"
	response.ModelID = "provider/model"
	response.Type = models.ModelTypeChat
	response.Enable = enabled
	response.ProviderID = "8b7bbec4-6069-45f4-a1cf-5f75e4a201ae"
	if window > 0 {
		response.Config.ContextWindow = &window
	}
	return response
}

func triggerTestProvider(clientType string) sqlc.Provider {
	return sqlc.Provider{ClientType: clientType}
}

func TestResolveTriggerConfigFillsBothBudgets(t *testing.T) {
	t.Parallel()

	cfg, err := ResolveTriggerConfig(
		context.Background(),
		triggerModelSourceStub{byID: map[string]models.GetResponse{"compact-model": triggerTestModel(200000, true)}},
		triggerProviderQueries{provider: triggerTestProvider("anthropic-messages")},
		triggerCredentialsStub{},
		"compact-model",
		"chat-model",
		"",
	)
	if err != nil {
		t.Fatalf("ResolveTriggerConfig() error = %v", err)
	}
	if cfg.SummaryWindowTokens != 200000 {
		t.Fatalf("SummaryWindowTokens = %d, want 200000", cfg.SummaryWindowTokens)
	}
	if cfg.MaxCompactTokens != 180000 {
		t.Fatalf("MaxCompactTokens = %d, want 180000", cfg.MaxCompactTokens)
	}
	if cfg.ModelID != "provider/model" || cfg.ClientType != "anthropic-messages" || cfg.APIKey != "resolved-key" || cfg.CodexAccountID != "resolved-codex" {
		t.Fatalf("resolved identity = %+v", cfg)
	}
	if cfg.ModelRecordID != "5f6f0e9c-8d31-4a51-9f6e-2f42f1f3b7aa" {
		t.Fatalf("ModelRecordID = %q, want the models row UUID for artifact provenance", cfg.ModelRecordID)
	}
}

func TestResolveTriggerConfigInheritsChatModel(t *testing.T) {
	t.Parallel()

	cfg, err := ResolveTriggerConfig(
		context.Background(),
		triggerModelSourceStub{byID: map[string]models.GetResponse{"chat-model": triggerTestModel(128000, true)}},
		triggerProviderQueries{provider: triggerTestProvider("openai-completions")},
		triggerCredentialsStub{},
		"",
		"chat-model",
		"",
	)
	if err != nil {
		t.Fatalf("ResolveTriggerConfig() error = %v", err)
	}
	if cfg.SummaryWindowTokens != 128000 {
		t.Fatalf("inherited SummaryWindowTokens = %d, want 128000", cfg.SummaryWindowTokens)
	}
}

func TestResolveTriggerConfigSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		compactModel string
		model        models.GetResponse
		clientType   string
		wantErr      error
	}{
		{
			name:    "no model configured",
			wantErr: ErrTriggerModelNotConfigured,
		},
		{
			name:         "model disabled",
			compactModel: "compact-model",
			model:        triggerTestModel(128000, false),
			clientType:   "anthropic-messages",
			wantErr:      ErrTriggerModelDisabled,
		},
		{
			name:         "provider without output limit",
			compactModel: "compact-model",
			model:        triggerTestModel(128000, true),
			clientType:   "openai-codex",
			wantErr:      ErrTriggerOutputLimitUnsupported,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveTriggerConfig(
				context.Background(),
				triggerModelSourceStub{byID: map[string]models.GetResponse{"compact-model": testCase.model}},
				triggerProviderQueries{provider: triggerTestProvider(testCase.clientType)},
				triggerCredentialsStub{},
				testCase.compactModel,
				"",
				"",
			)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("ResolveTriggerConfig() error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

func TestResolveTriggerConfigFallsBackToLatestSessionModel(t *testing.T) {
	t.Parallel()

	const latestID = "9d3f65a2-7c11-4bb6-8d5e-0e6cf19d4c21"
	latest := triggerTestModel(64000, true)
	latest.ID = latestID
	cfg, err := ResolveTriggerConfig(
		context.Background(),
		triggerModelSourceStub{byID: map[string]models.GetResponse{latestID: latest}},
		triggerProviderQueries{
			provider:           triggerTestProvider("anthropic-messages"),
			latestSessionModel: mustTriggerUUID(t, latestID),
		},
		triggerCredentialsStub{},
		"",
		"",
		"7b1e2f7e-3d3a-4a90-9a51-3a1f0d5f60cd",
	)
	if err != nil {
		t.Fatalf("ResolveTriggerConfig() error = %v", err)
	}
	if cfg.ModelRecordID != latestID {
		t.Fatalf("ModelRecordID = %q, want the session's latest model %q", cfg.ModelRecordID, latestID)
	}
}

func TestResolveTriggerConfigRejectsNonChatModel(t *testing.T) {
	t.Parallel()

	embedding := triggerTestModel(64000, true)
	embedding.Type = models.ModelTypeEmbedding
	_, err := ResolveTriggerConfig(
		context.Background(),
		triggerModelSourceStub{byID: map[string]models.GetResponse{"compact-model": embedding}},
		triggerProviderQueries{provider: triggerTestProvider("anthropic-messages")},
		triggerCredentialsStub{},
		"compact-model",
		"",
		"",
	)
	if !errors.Is(err, ErrTriggerModelNotChat) {
		t.Fatalf("ResolveTriggerConfig() error = %v, want ErrTriggerModelNotChat", err)
	}
}

func mustTriggerUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	parsed, err := db.ParseUUID(id)
	if err != nil {
		t.Fatalf("ParseUUID(%q): %v", id, err)
	}
	return parsed
}

func TestResolveTriggerConfigToleratesUnknownWindow(t *testing.T) {
	t.Parallel()

	cfg, err := ResolveTriggerConfig(
		context.Background(),
		triggerModelSourceStub{byID: map[string]models.GetResponse{"compact-model": triggerTestModel(0, true)}},
		triggerProviderQueries{provider: triggerTestProvider("anthropic-messages")},
		triggerCredentialsStub{},
		"compact-model",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("ResolveTriggerConfig() error = %v", err)
	}
	if cfg.SummaryWindowTokens != 0 || cfg.MaxCompactTokens != 0 {
		t.Fatalf("unknown window budgets = window:%d max:%d, want zero so the engine keeps its conservative defaults", cfg.SummaryWindowTokens, cfg.MaxCompactTokens)
	}
}
