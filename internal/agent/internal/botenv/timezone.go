package botenv

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/timezone"
)

// ResolveTimezone resolves the effective timezone for a request.
// Priority: bot timezone > user timezone > system default.
func (e *Env) ResolveTimezone(ctx context.Context, botID, userID string) (string, *time.Location) {
	fallbackName, fallbackLocation := e.systemTimezoneDefaults()

	// 1. Try bot timezone first.
	if name, loc, ok := e.loadBotTimezone(ctx, botID); ok {
		return name, loc
	}

	// 2. Fall back to user timezone.
	if name, loc, ok := e.loadUserTimezone(ctx, userID); ok {
		return name, loc
	}

	return fallbackName, fallbackLocation
}

func (e *Env) systemTimezoneDefaults() (string, *time.Location) {
	return e.clockLocation.String(), e.clockLocation
}

func (e *Env) loadBotTimezone(ctx context.Context, botID string) (string, *time.Location, bool) {
	if strings.TrimSpace(botID) == "" {
		return "", nil, false
	}
	botUUID, err := db.ParseUUID(botID)
	if err != nil {
		return "", nil, false
	}
	row, err := e.queries.GetBotByID(ctx, botUUID)
	if err != nil {
		return "", nil, false
	}
	tz := ""
	if row.Timezone.Valid {
		tz = strings.TrimSpace(row.Timezone.String)
	}
	if tz == "" {
		return "", nil, false
	}
	loc, name, err := timezone.Resolve(tz)
	if err != nil {
		e.logger.WarnContext(ctx, "resolve bot timezone failed",
			slog.String("bot_id", botID),
			slog.String("timezone", tz),
			slog.Any("error", err),
		)
		return "", nil, false
	}
	return name, loc, true
}

func (e *Env) loadUserTimezone(ctx context.Context, userID string) (string, *time.Location, bool) {
	if strings.TrimSpace(userID) == "" {
		return "", nil, false
	}
	account, err := e.accounts.Get(ctx, strings.TrimSpace(userID))
	if err != nil {
		return "", nil, false
	}
	tz := strings.TrimSpace(account.Timezone)
	if tz == "" {
		return "", nil, false
	}
	loc, name, err := timezone.Resolve(tz)
	if err != nil {
		e.logger.WarnContext(ctx, "resolve user timezone failed",
			slog.String("user_id", userID),
			slog.String("timezone", tz),
			slog.Any("error", err),
		)
		return "", nil, false
	}
	return name, loc, true
}
