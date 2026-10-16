package application

import (
	"context"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/oauthctx"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
)

// runBudgetCompactionSync compacts everything before the protected tail, so
// the session history can fit historyBudget. A failure leaves the history as
// it was; the caller reports the budget it could not satisfy.
func (s *Service) runBudgetCompactionSync(ctx context.Context, req ChatRequest, inputTokens, historyBudget int, turnModelID string) compaction.Result {
	if ctx.Err() != nil {
		return compaction.Result{}
	}
	if s.compactionService == nil || s.settingsService == nil {
		s.logger.WarnContext(ctx, "compaction sync: skipped, service or settings nil")
		return compaction.Result{}
	}
	botSettings, err := s.settingsService.GetBot(ctx, req.BotID)
	if err != nil {
		s.logger.WarnContext(ctx, "compaction sync: failed to load settings", slog.Any("error", err))
		return compaction.Result{}
	}
	cfg, err := s.buildCompactionConfig(ctx, req, botSettings, inputTokens, turnModelID)
	if err != nil {
		s.logger.WarnContext(ctx, "compaction sync: failed to build config", slog.Any("error", err))
		return compaction.Result{}
	}
	if cfg.ModelID == "" {
		return compaction.Result{}
	}
	cfg.ProtectedSources = compactionProtectedSources(req)
	cfg.CandidatesFrom = req.historyViewFrom
	cfg.Ratio = 100
	cfg.HistoryBudgetTokens = historyBudget

	s.logger.InfoContext(ctx, "compaction sync: running synchronously",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", inputTokens),
		slog.String("model_id", cfg.ModelID),
	)
	res, err := s.compactionService.RunCompactionSync(ctx, cfg)
	if err != nil {
		s.logger.WarnContext(ctx, "compaction sync: failed", slog.Any("error", err))
		return compaction.Result{}
	}
	s.logger.InfoContext(ctx, "compaction sync: finished",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.String("status", res.Status),
	)
	return res
}

// buildCompactionConfig resolves the summarizer through the shared model
// policy — explicit override first, then the turn's actually-resolved model,
// then the bot chat default, then the session's latest model — and attaches
// credentials. Unavailable-model conditions return an empty config; infrastructure
// errors propagate.
func (s *Service) buildCompactionConfig(ctx context.Context, req ChatRequest, botSettings settings.Settings, inputTokens int, turnModelID string) (compaction.TriggerConfig, error) {
	sessionModelID := ""
	if strings.TrimSpace(botSettings.CompactionModelID) == "" && strings.TrimSpace(turnModelID) == "" && strings.TrimSpace(botSettings.ChatModelID) == "" {
		sessionModelID = models.LatestSessionModelID(ctx, s.queries, req.ThreadID)
	}
	resolution, err := models.ResolveCompactionModel(
		ctx,
		s.modelsService,
		s.queries,
		botSettings.CompactionModelID,
		turnModelID,
		botSettings.ChatModelID,
		sessionModelID,
	)
	if models.IsCompactionModelUnavailable(err) {
		s.logger.InfoContext(ctx, "compaction: skipped",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Any("reason", err),
		)
		return compaction.TriggerConfig{}, nil
	}
	if err != nil {
		return compaction.TriggerConfig{}, err
	}
	authService := providers.NewService(nil, s.queries, "")
	authCtx := oauthctx.WithUserID(ctx, req.UserID)
	creds, err := authService.ResolveModelCredentials(authCtx, resolution.Provider)
	if err != nil {
		return compaction.TriggerConfig{}, err
	}
	cfg := compaction.NewTriggerConfig(compaction.TriggerModel{
		Slug:                  resolution.Model.ModelID,
		RecordID:              resolution.Model.ID,
		ClientType:            resolution.Provider.ClientType,
		APIKey:                creds.APIKey,
		CodexAccountID:        creds.CodexAccountID,
		BaseURL:               providers.ProviderConfigString(resolution.Provider, "base_url"),
		ChatCompletionsCompat: providers.ProviderConfigString(resolution.Provider, models.ChatCompletionsCompatConfigKey),
		PromptCacheTTL:        providers.ProviderConfigString(resolution.Provider, "prompt_cache_ttl"),
		WindowTokens:          resolution.WindowTokens,
	})
	cfg.BotID = req.BotID
	cfg.SessionID = req.ThreadID
	cfg.TotalInputTokens = inputTokens
	cfg.HTTPClient = s.compactionHTTPClient
	return cfg, nil
}

// compactionProtectedSources is the current input a compaction must keep raw.
// Discuss callers pass the protected suffix their admission chose, so older
// unconsumed input stays compactable.
func compactionProtectedSources(req ChatRequest) []turn.ContextMessageSource {
	sources := append([]turn.ContextMessageSource(nil), req.discussCurrentSources...)
	if req.ExternalMessageID != "" {
		sources = append(sources, turn.ContextMessageSource{Kind: "external", ID: req.ExternalMessageID, Current: true})
	}
	for _, id := range []string{req.RequiredHistoryMessageID, req.PersistedUserMessageID} {
		if id != "" {
			sources = append(sources, turn.ContextMessageSource{Kind: "history", ID: id, Current: true})
		}
	}
	return sources
}
