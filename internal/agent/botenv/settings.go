package botenv

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/settings"
)

func (e *Env) LoadSettings(ctx context.Context, botID string) (settings.Settings, error) {
	return e.settings.GetBot(ctx, botID)
}

func (e *Env) LoadRuntimeInfo(ctx context.Context, botID string) (native.BotInfo, bool) {
	info := native.BotInfo{ID: strings.TrimSpace(botID)}
	botUUID, err := db.ParseUUID(botID)
	if err != nil {
		return info, false
	}
	row, err := e.queries.GetBotByID(ctx, botUUID)
	if err != nil {
		e.logger.DebugContext(ctx, "failed to load bot metadata for loop detection",
			slog.String("bot_id", botID),
			slog.Any("error", err),
		)
		return info, false
	}
	info.Name = strings.TrimSpace(row.Name)
	if row.DisplayName.Valid {
		info.DisplayName = strings.TrimSpace(row.DisplayName.String)
	}
	if row.Timezone.Valid {
		info.Timezone = strings.TrimSpace(row.Timezone.String)
	}
	return info, parseLoopDetectionEnabledFromMetadata(row.Metadata)
}

func parseLoopDetectionEnabledFromMetadata(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var metadata map[string]any
	if err := json.Unmarshal(payload, &metadata); err != nil || metadata == nil {
		return false
	}
	features, ok := metadata["features"].(map[string]any)
	if !ok {
		return false
	}
	loopDetection, ok := features["loop_detection"].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := loopDetection["enabled"].(bool)
	if !ok {
		return false
	}
	return enabled
}
