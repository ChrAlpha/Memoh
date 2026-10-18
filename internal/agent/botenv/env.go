// Package botenv resolves what a run needs to know about the bot it acts for:
// its settings, time zone, display names and identities, and the lifecycle
// hooks configured in its workspace.
package botenv

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/hooks"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/workspace/bridge"
)

// Queries is the part of the database botenv reads.
type Queries interface {
	GetBotByID(ctx context.Context, id pgtype.UUID) (sqlc.GetBotByIDRow, error)
	GetChannelIdentityByID(ctx context.Context, id pgtype.UUID) (sqlc.ChannelIdentity, error)
	GetUserByID(ctx context.Context, userID pgtype.UUID) (sqlc.User, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the component's own records.
	Logger *slog.Logger
	// Queries reads bot, identity and user rows.
	Queries Queries
	// Settings resolves per-bot settings.
	Settings *settings.Service
	// Accounts resolves a user's time zone.
	Accounts *accounts.Service
	// Bridge reaches the bot's workspace for the hook environment. A provider
	// that cannot describe the workspace leaves the container defaults.
	Bridge bridge.Provider
	// Hooks runs user-defined hooks. Without it every hook event is allowed.
	Hooks *hooks.Service
	// ClockLocation is the system default time zone; nil means UTC.
	ClockLocation *time.Location
}

// Env answers questions about a bot's environment for one run.
type Env struct {
	logger        *slog.Logger
	queries       Queries
	settings      *settings.Service
	accounts      *accounts.Service
	bridge        bridge.Provider
	hooks         hookRunner
	clockLocation *time.Location
}

// New returns an Env.
func New(d Deps) *Env {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	clockLocation := d.ClockLocation
	if clockLocation == nil {
		clockLocation = time.UTC
	}
	var runner hookRunner = noHooks{}
	if d.Hooks != nil {
		runner = serviceHooks{service: d.Hooks}
	}
	return &Env{
		logger:        logger,
		queries:       d.Queries,
		settings:      d.Settings,
		accounts:      d.Accounts,
		bridge:        d.Bridge,
		hooks:         runner,
		clockLocation: clockLocation,
	}
}
