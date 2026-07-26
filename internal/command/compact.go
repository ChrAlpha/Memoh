package command

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/memohai/memoh/internal/agent/context/compaction"
	"github.com/memohai/memoh/internal/db"
)

// errCompactNoModel is a sentinel returned by buildCompactConfig when neither
// a compaction model nor a chat model is configured. The Handler catches it
// via errors.Is and surfaces a localized user message; other (internal) errors
// flow through friendlyCommandError's looksLikeInternalError path.
var errCompactNoModel = errors.New("compact: no compaction or chat model configured")

func (h *Handler) buildCompactGroup() *CommandGroup {
	g := newCommandGroup("compact", "Compact conversation context")
	g.DefaultAction = "run"
	g.Register(SubCommand{
		Name:    "run",
		Usage:   "run - Compact the current session's context immediately",
		IsWrite: true,
		Handler: func(cc CommandContext) (string, error) {
			if h.compactionService == nil {
				return cc.T("cmd.compact.unavailable"), nil
			}
			sessionID := cc.SessionID
			if sessionID == "" {
				botUUID, err := db.ParseUUID(cc.BotID)
				if err != nil {
					// cc.BotID is framework-set so this only fires if the
					// framework injects a malformed UUID — a deep internal
					// bug. Log the diagnostic and surface a generic friendly
					// message rather than leaking "invalid UUID length: 5"
					// to the user verbatim.
					if h.logger != nil {
						h.logger.Warn("compact: parse bot id failed", slog.String("bot_id", cc.BotID), slog.Any("error", err))
					}
					return cc.T("cmd.error.generic", map[string]any{"command": CmdRef("compact")}), nil
				}
				latestUUID, err := h.queries.GetLatestSessionIDByBot(cc.Ctx, botUUID)
				if err != nil {
					return cc.T("cmd.session.noActive"), nil
				}
				sessionID = uuid.UUID(latestUUID.Bytes).String()
			}

			cfg, err := h.buildCompactConfig(cc, sessionID)
			if err != nil {
				if errors.Is(err, errCompactNoModel) {
					return cc.T("cmd.compact.noModel"), nil
				}
				return "", err
			}

			res, err := h.compactionService.RunCompactionSync(cc.Ctx, cfg)
			if err != nil {
				return "", fmt.Errorf("compaction failed: %w", err)
			}
			if res.Status != compaction.StatusOK {
				return cc.T("cmd.compact.noop"), nil
			}
			return cc.T("cmd.compact.done"), nil
		},
	})
	return g
}

func (h *Handler) buildCompactConfig(cc CommandContext, sessionID string) (compaction.TriggerConfig, error) {
	botSettings, err := h.settingsService.GetBot(cc.Ctx, cc.BotID)
	if err != nil {
		return compaction.TriggerConfig{}, fmt.Errorf("failed to load settings: %w", err)
	}
	cfg, err := compaction.ResolveTriggerConfig(
		cc.Ctx,
		h.modelsService,
		h.sqlcQueries,
		h.providersService,
		botSettings.CompactionModelID,
		botSettings.ChatModelID,
		sessionID,
	)
	if errors.Is(err, compaction.ErrTriggerModelNotConfigured) {
		return compaction.TriggerConfig{}, errCompactNoModel
	}
	if err != nil {
		return compaction.TriggerConfig{}, err
	}
	cfg.BotID = cc.BotID
	cfg.SessionID = sessionID
	cfg.Ratio = 100
	cfg.TotalInputTokens = 1
	cfg.Manual = true
	return cfg, nil
}
