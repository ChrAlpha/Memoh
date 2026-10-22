package agenttest

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/internal/botenv"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/settings"
)

// BotSettingsQueries answers the one settings read a run makes with a bot whose
// settings are all defaults. Every other query panics.
type BotSettingsQueries struct {
	dbstore.Queries
}

func (BotSettingsQueries) GetSettingsByBotID(_ context.Context, botID pgtype.UUID) (sqlc.GetSettingsByBotIDRow, error) {
	return sqlc.GetSettingsByBotIDRow{BotID: botID, ReasoningEffort: "medium"}, nil
}

// NoBotQueries is a database with no bots, identities or users.
type NoBotQueries struct{}

func (NoBotQueries) GetBotByID(context.Context, pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return sqlc.GetBotByIDRow{}, pgx.ErrNoRows
}

func (NoBotQueries) GetChannelIdentityByID(context.Context, pgtype.UUID) (sqlc.ChannelIdentity, error) {
	return sqlc.ChannelIdentity{}, pgx.ErrNoRows
}

func (NoBotQueries) GetUserByID(context.Context, pgtype.UUID) (sqlc.User, error) {
	return sqlc.User{}, pgx.ErrNoRows
}

// BotEnv is a bot environment over a database with no bots or users: settings
// are the defaults, time zones fall back to UTC and no identity resolves.
func BotEnv(logger *slog.Logger) *botenv.Env {
	return BotEnvWithSettings(logger, settings.NewService(logger, BotSettingsQueries{}, nil, nil))
}

// BotEnvWithSettings is BotEnv with the settings a test supplies.
func BotEnvWithSettings(logger *slog.Logger, svc *settings.Service) *botenv.Env {
	return botenv.New(botenv.Deps{
		Logger:   logger,
		Queries:  NoBotQueries{},
		Settings: svc,
		Accounts: accounts.NewService(logger, nil),
	})
}

// EmptyStore is a database that holds no sessions and records nothing.
type EmptyStore struct{}

func (EmptyStore) GetSessionByID(context.Context, pgtype.UUID) (sqlc.BotSession, error) {
	return sqlc.BotSession{}, pgx.ErrNoRows
}

func (EmptyStore) GetTurnAgentTurnID(context.Context, sqlc.GetTurnAgentTurnIDParams) (string, error) {
	return "", pgx.ErrNoRows
}

func (EmptyStore) UpdateSessionModelPreference(context.Context, sqlc.UpdateSessionModelPreferenceParams) error {
	return nil
}

func (EmptyStore) DeleteRuntimeDecisionProjectionsByRun(context.Context, sqlc.DeleteRuntimeDecisionProjectionsByRunParams) (int64, error) {
	return 0, nil
}
