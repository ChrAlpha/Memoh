package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/bots"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/providers"
	"github.com/memohai/memoh/internal/settings"
)

type compactionCapabilityQueries struct {
	dbstore.Queries
	bot      sqlc.GetBotByIDRow
	model    sqlc.Model
	provider sqlc.Provider
}

func (q *compactionCapabilityQueries) GetBotByID(context.Context, pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return q.bot, nil
}

func (q *compactionCapabilityQueries) GetSettingsByBotID(context.Context, pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{
		Language:           settings.DefaultLanguage,
		ReasoningEffort:    settings.DefaultReasoningEffort,
		HeartbeatInterval:  settings.DefaultHeartbeatInterval,
		CompactionRatio:    80,
		CompactionModelID:  q.model.ID,
		CommandUiLanguage:  settings.DefaultCommandUILanguage,
		ChatAcpProjectPath: settings.DefaultACPProjectPath,
		ChatAcpProjectMode: settings.DefaultACPProjectMode,
	}, nil
}

func (q *compactionCapabilityQueries) GetModelByID(context.Context, pgtype.UUID) (sqlc.Model, error) {
	return q.model, nil
}

func (q *compactionCapabilityQueries) GetProviderByID(context.Context, pgtype.UUID) (sqlc.Provider, error) {
	return q.provider, nil
}

func (*compactionCapabilityQueries) GetProviderOAuthTokenByProvider(context.Context, pgtype.UUID) (sqlc.ProviderOauthToken, error) {
	return sqlc.ProviderOauthToken{}, pgx.ErrNoRows
}

func TestTriggerCompactRejectsProviderWithoutOutputLimitBeforeService(t *testing.T) {
	t.Parallel()

	botID := "00000000-0000-0000-0000-000000000423"
	userID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	modelID := testUUID("00000000-0000-0000-0000-000000000421")
	providerID := testUUID("00000000-0000-0000-0000-000000000422")
	queries := &compactionCapabilityQueries{
		bot: testBotRow(botID, map[string]any{}),
		model: sqlc.Model{
			ID:         modelID,
			ModelID:    "codex-compact-model",
			ProviderID: providerID,
			Type:       string(models.ModelTypeChat),
			Enable:     true,
		},
		provider: sqlc.Provider{
			ID:         providerID,
			Name:       "codex-provider",
			ClientType: string(models.ClientTypeOpenAICodex),
			Enable:     true,
		},
	}
	logger := slog.New(slog.DiscardHandler)
	handler := NewCompactionHandler(
		logger,
		nil,
		bots.NewService(logger, queries),
		newTestAdminAccountService("admin"),
		settings.NewService(logger, queries, nil, nil),
		models.NewService(logger, queries),
		queries,
		providers.NewService(logger, queries, ""),
	)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bots/"+botID+"/sessions/00000000-0000-0000-0000-000000000424/compact", nil)
	recorder := httptest.NewRecorder()
	e := echo.New()
	echoCtx := testAuthContext(e, req, recorder, userID)
	echoCtx.SetPath("/bots/:bot_id/sessions/:session_id/compact")
	echoCtx.SetParamNames("bot_id", "session_id")
	echoCtx.SetParamValues(botID, "00000000-0000-0000-0000-000000000424")

	err := handler.TriggerCompact(echoCtx)
	if got := apperror.CodeOf(err); got != apperror.CodeCompactionModelUnavailable {
		t.Fatalf("TriggerCompact() code = %q, want %q (err=%v)", got, apperror.CodeCompactionModelUnavailable, err)
	}
	public, ok := apperror.PublicFrom(err, "req-test")
	if !ok || public.Args["reason"] != "output_limit_unsupported" {
		t.Fatalf("TriggerCompact() public = %+v ok=%t, want output_limit_unsupported reason", public, ok)
	}
}
