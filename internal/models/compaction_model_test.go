package models

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

type compactionModelQueries struct {
	dbstore.Queries
	models             map[pgtype.UUID]sqlc.Model
	provider           sqlc.Provider
	latestSessionModel pgtype.UUID
}

func (q *compactionModelQueries) GetModelByID(_ context.Context, id pgtype.UUID) (sqlc.Model, error) {
	model, ok := q.models[id]
	if !ok {
		return sqlc.Model{}, pgx.ErrNoRows
	}
	return model, nil
}

func (q *compactionModelQueries) GetProviderByID(_ context.Context, _ pgtype.UUID) (sqlc.Provider, error) {
	return q.provider, nil
}

func (q *compactionModelQueries) GetLatestSessionModelID(_ context.Context, _ pgtype.UUID) (pgtype.UUID, error) {
	return q.latestSessionModel, nil
}

func compactionTestUUID(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	parsed, err := db.ParseUUID(id)
	if err != nil {
		t.Fatalf("ParseUUID(%q): %v", id, err)
	}
	return parsed
}

func compactionTestModel(t *testing.T, id, slug string, window int, enabled bool) sqlc.Model {
	t.Helper()
	config := []byte(`{"context_window":200000}`)
	if window == 0 {
		config = []byte(`{}`)
	}
	return sqlc.Model{
		ID:         compactionTestUUID(t, id),
		ModelID:    slug,
		ProviderID: compactionTestUUID(t, "00000000-0000-0000-0000-00000000f001"),
		Type:       "chat",
		Enable:     enabled,
		Config:     config,
	}
}

func compactionTestProvider(clientType string) sqlc.Provider {
	return sqlc.Provider{Name: "test-provider", ClientType: clientType, Enable: true}
}

func compactionResolverEnv(t *testing.T, queries *compactionModelQueries) *Service {
	t.Helper()
	return NewService(slog.New(slog.DiscardHandler), queries)
}

func TestResolveCompactionModelPrefersEarlierCandidates(t *testing.T) {
	t.Parallel()

	const overrideID = "00000000-0000-0000-0000-00000000a001"
	const turnID = "00000000-0000-0000-0000-00000000a002"
	queries := &compactionModelQueries{
		models: map[pgtype.UUID]sqlc.Model{
			compactionTestUUID(t, overrideID): compactionTestModel(t, overrideID, "override/model", 200000, true),
			compactionTestUUID(t, turnID):     compactionTestModel(t, turnID, "turn/model", 200000, true),
		},
		provider: compactionTestProvider("anthropic-messages"),
	}
	svc := compactionResolverEnv(t, queries)

	resolution, err := ResolveCompactionModel(context.Background(), svc, queries, overrideID, turnID)
	if err != nil {
		t.Fatalf("ResolveCompactionModel() error = %v", err)
	}
	if resolution.Model.ID != overrideID {
		t.Fatalf("resolved = %s, want the explicit override first", resolution.Model.ID)
	}

	resolution, err = ResolveCompactionModel(context.Background(), svc, queries, "", turnID)
	if err != nil {
		t.Fatalf("ResolveCompactionModel() fallback error = %v", err)
	}
	if resolution.Model.ID != turnID {
		t.Fatalf("resolved = %s, want the turn model when no override is set", resolution.Model.ID)
	}
	if resolution.WindowTokens != 200000 {
		t.Fatalf("WindowTokens = %d, want the declared window", resolution.WindowTokens)
	}
}

func TestResolveCompactionModelSentinels(t *testing.T) {
	t.Parallel()

	const modelID = "00000000-0000-0000-0000-00000000a003"
	cases := []struct {
		name       string
		model      sqlc.Model
		provider   sqlc.Provider
		candidates []string
		wantErr    error
	}{
		{
			name:    "no candidates",
			wantErr: ErrCompactionModelNotConfigured,
		},
		{
			name: "not a chat model",
			model: func() sqlc.Model {
				m := compactionTestModel(t, modelID, "embed/model", 200000, true)
				m.Type = "embedding"
				return m
			}(),
			provider:   compactionTestProvider("anthropic-messages"),
			candidates: []string{modelID},
			wantErr:    ErrCompactionModelNotChat,
		},
		{
			name:       "model disabled",
			model:      compactionTestModel(t, modelID, "off/model", 200000, false),
			provider:   compactionTestProvider("anthropic-messages"),
			candidates: []string{modelID},
			wantErr:    ErrCompactionModelDisabled,
		},
		{
			name:  "provider disabled",
			model: compactionTestModel(t, modelID, "prov/model", 200000, true),
			provider: func() sqlc.Provider {
				p := compactionTestProvider("anthropic-messages")
				p.Enable = false
				return p
			}(),
			candidates: []string{modelID},
			wantErr:    ErrCompactionProviderDisabled,
		},
		{
			name:       "provider ignores output limits",
			model:      compactionTestModel(t, modelID, "codex/model", 200000, true),
			provider:   compactionTestProvider("openai-codex"),
			candidates: []string{modelID},
			wantErr:    ErrCompactionOutputLimitUnsupported,
		},
		{
			name:       "window unknown fails closed",
			model:      compactionTestModel(t, modelID, "nowin/model", 0, true),
			provider:   compactionTestProvider("anthropic-messages"),
			candidates: []string{modelID},
			wantErr:    ErrCompactionWindowUnknown,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			queries := &compactionModelQueries{
				models:   map[pgtype.UUID]sqlc.Model{testCase.model.ID: testCase.model},
				provider: testCase.provider,
			}
			svc := compactionResolverEnv(t, queries)
			_, err := ResolveCompactionModel(context.Background(), svc, queries, testCase.candidates...)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("ResolveCompactionModel() error = %v, want %v", err, testCase.wantErr)
			}
			if !IsCompactionModelUnavailable(err) {
				t.Fatalf("IsCompactionModelUnavailable(%v) = false, want true", err)
			}
		})
	}
}

func TestLatestSessionModelIDFallback(t *testing.T) {
	t.Parallel()

	const latest = "00000000-0000-0000-0000-00000000a004"
	queries := &compactionModelQueries{latestSessionModel: compactionTestUUID(t, latest)}
	if got := LatestSessionModelID(context.Background(), queries, "00000000-0000-0000-0000-00000000a005"); got != latest {
		t.Fatalf("LatestSessionModelID() = %q, want %q", got, latest)
	}
	if got := LatestSessionModelID(context.Background(), queries, "not-a-uuid"); got != "" {
		t.Fatalf("LatestSessionModelID(invalid) = %q, want empty", got)
	}
}
