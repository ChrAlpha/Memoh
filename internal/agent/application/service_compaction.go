package application

import (
	"context"
	"log/slog"
	"strings"

	"github.com/memohai/memoh/internal/agent/context/compaction"
	"github.com/memohai/memoh/internal/oauthctx"
	"github.com/memohai/memoh/internal/providers"
	"github.com/memohai/memoh/internal/settings"
)

// compactionBudgetThresholdPercent is the shared budget share at which
// compaction triggers: the pre-send synchronous backstop fires when
// compactable history reaches it, and async triggers clamp the user
// threshold to it so they fire before the blocking backstop does.
const compactionBudgetThresholdPercent = 70

// Model-relative policy: when the configured absolute threshold is zero, the
// controller derives every level from the chat model's context window. The
// async maintenance pass fires at the soft share, the blocking pre-send
// backstop waits for the hard share, and both compact the raw tail down to
// the target share — three separate roles so background maintenance normally
// keeps the backstop from ever blocking a turn.
const (
	compactionSoftThresholdPercent = 60
	compactionHardThresholdPercent = 75
	compactionTargetPercent        = 45
)

// modelRelativeCompaction reports whether the bot runs the model-relative
// default policy instead of a legacy absolute threshold.
func modelRelativeCompaction(userThreshold int) bool {
	return userThreshold <= 0
}

// autoCompactionThreshold is the async trigger level: the soft window share
// under the model-relative policy, or the clamped absolute threshold in
// legacy mode. Zero disables the async trigger.
func autoCompactionThreshold(userThreshold, contextTokenBudget int) int {
	if modelRelativeCompaction(userThreshold) {
		if contextTokenBudget <= 0 {
			return 0
		}
		return contextTokenBudget * compactionSoftThresholdPercent / 100
	}
	return effectiveCompactionThreshold(userThreshold, contextTokenBudget)
}

// compactionTargetTokensFor is the post-compaction raw-tail goal shared by the
// async and synchronous paths, so both modes converge on the same pressure.
func compactionTargetTokensFor(userThreshold, ratio, contextTokenBudget int) int {
	if modelRelativeCompaction(userThreshold) {
		if contextTokenBudget <= 0 {
			return 0
		}
		return contextTokenBudget * compactionTargetPercent / 100
	}
	return syncCompactionTargetTokens(contextTokenBudget, ratio)
}

// syncCompactionShouldRun gates the blocking backstop: model-relative bots
// wait for the hard share so the soft async pass gets a chance first; legacy
// bots keep the caller's shared-budget gate.
func syncCompactionShouldRun(userThreshold, pressure, contextTokenBudget int) bool {
	if !modelRelativeCompaction(userThreshold) {
		return true
	}
	if contextTokenBudget <= 0 {
		return false
	}
	return pressure >= contextTokenBudget*compactionHardThresholdPercent/100
}

// effectiveCompactionThreshold clamps the user-configured absolute threshold
// to the budget share, so an absolute default (e.g. 100000) still fires on
// models whose context window never reaches it. A non-positive threshold
// keeps async compaction disabled.
func effectiveCompactionThreshold(threshold, contextTokenBudget int) int {
	if threshold <= 0 || contextTokenBudget <= 0 {
		return threshold
	}
	budgetThreshold := contextTokenBudget * compactionBudgetThresholdPercent / 100
	if budgetThreshold > 0 && budgetThreshold < threshold {
		return budgetThreshold
	}
	return threshold
}

func asyncCompactionInputTokens(rc resolvedContext, providerInputTokens int) int {
	if rc.compactableTokensKnown {
		return rc.compactableTokens
	}
	return providerInputTokens
}

func (s *Service) maybeCompact(ctx context.Context, req ChatRequest, rc resolvedContext, inputTokens int) {
	done := s.enterSessionCompaction(req.BotID, req.ThreadID)
	defer done()
	inputTokens = asyncCompactionInputTokens(rc, inputTokens)
	if s.compactionService == nil || s.settingsService == nil {
		s.logger.Info("compaction: skipped, service or settings nil")
		return
	}
	botSettings, err := s.settingsService.GetBot(ctx, req.BotID)
	if err != nil {
		s.logger.Warn("compaction: failed to load settings", slog.Any("error", err))
		return
	}
	if !botSettings.CompactionEnabled {
		s.logger.Info("compaction: skipped, disabled")
		return
	}
	threshold := autoCompactionThreshold(botSettings.CompactionThreshold, rc.contextTokenBudget)
	if threshold <= 0 {
		s.logger.Info("compaction: skipped, no usable threshold",
			slog.Int("configured_threshold", botSettings.CompactionThreshold),
			slog.Int("context_token_budget", rc.contextTokenBudget),
		)
		return
	}
	if !compaction.ShouldCompact(inputTokens, threshold) {
		s.logger.Info("compaction: skipped, below threshold",
			slog.Int("input_tokens", inputTokens),
			slog.Int("threshold", threshold),
		)
		return
	}

	s.logger.Info("compaction: triggering",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", inputTokens),
		slog.Int("threshold", threshold),
		slog.Int("ratio", botSettings.CompactionRatio),
	)

	cfg, err := s.buildCompactionConfig(ctx, req, botSettings, inputTokens)
	if err != nil {
		s.logger.Warn("compaction: failed to build config", slog.Any("error", err))
		return
	}
	if cfg.ModelID == "" {
		// buildCompactionConfig returns an empty cfg when no compaction model
		// is configured or the configured one is disabled. Skip the trigger
		// so the compaction service doesn't run hooks + fail on empty UUIDs.
		return
	}
	cfg.TargetTokens = compactionTargetTokensFor(botSettings.CompactionThreshold, cfg.Ratio, rc.contextTokenBudget)
	if err := s.compactionService.RunCompaction(ctx, cfg); err != nil {
		s.logger.Error("compaction failed", slog.String("bot_id", cfg.BotID), slog.String("session_id", cfg.SessionID), slog.Any("error", err))
	}
}

// runCompactionSync runs compaction synchronously when context reaches
// 70% of the model's context window and reports the session-scoped result.
// A noop (failure cooldown, another compaction in flight, or nothing to
// compact) leaves this turn's context untouched: the request proceeds as-is,
// possibly still above the threshold, and the next turn re-evaluates.
func (s *Service) runCompactionSync(ctx context.Context, req ChatRequest, inputTokens, contextTokenBudget int) compaction.Result {
	if s.compactionService == nil || s.settingsService == nil {
		s.logger.Warn("compaction sync: skipped, service or settings nil")
		return compaction.Result{}
	}
	botSettings, err := s.settingsService.GetBot(ctx, req.BotID)
	if err != nil {
		s.logger.Warn("compaction sync: failed to load settings", slog.Any("error", err))
		return compaction.Result{}
	}
	if !botSettings.CompactionEnabled {
		s.logger.Warn("compaction sync: compaction disabled, skipping")
		return compaction.Result{}
	}
	if !syncCompactionShouldRun(botSettings.CompactionThreshold, inputTokens, contextTokenBudget) {
		s.logger.Info("compaction sync: below the hard threshold, leaving maintenance to the async pass",
			slog.Int("input_tokens", inputTokens),
			slog.Int("context_token_budget", contextTokenBudget),
		)
		return compaction.Result{}
	}

	cfg, err := s.buildCompactionConfig(ctx, req, botSettings, inputTokens)
	if err != nil {
		s.logger.Warn("compaction sync: failed to build config", slog.Any("error", err))
		return compaction.Result{}
	}
	if cfg.ModelID == "" {
		// Same skip path as the async trigger above — no model or model
		// disabled means there is nothing to compact.
		return compaction.Result{}
	}
	cfg.TargetTokens = compactionTargetTokensFor(botSettings.CompactionThreshold, cfg.Ratio, contextTokenBudget)

	s.logger.Info("compaction sync: running synchronously",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", inputTokens),
		slog.String("model_id", cfg.ModelID),
	)

	done := s.enterSessionCompactionForStream(req.BotID, req.ThreadID, strings.TrimSpace(req.StreamID))
	defer done()
	res, err := s.compactionService.RunCompactionSync(ctx, cfg)
	if err != nil {
		s.logger.Warn("compaction sync: failed", slog.Any("error", err))
		return compaction.Result{}
	}
	s.logger.Info("compaction sync: finished",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.String("status", res.Status),
	)
	return res
}

// buildCompactionConfig resolves the compaction engine config through the
// shared trigger resolver: model (compaction override or chat model), provider
// credentials, and both token budgets. Unavailable-model conditions stand the
// automatic trigger down silently; infrastructure errors propagate.
func (s *Service) buildCompactionConfig(ctx context.Context, req ChatRequest, botSettings settings.Settings, inputTokens int) (compaction.TriggerConfig, error) {
	ratio := botSettings.CompactionRatio
	if ratio <= 0 || ratio > 100 {
		ratio = 80
	}
	authService := providers.NewService(nil, s.queries, "")
	authCtx := oauthctx.WithUserID(ctx, req.UserID)
	cfg, err := compaction.ResolveTriggerConfig(
		authCtx,
		s.modelsService,
		s.queries,
		authService,
		botSettings.CompactionModelID,
		botSettings.ChatModelID,
		req.ThreadID,
	)
	if compaction.IsTriggerConfigUnavailable(err) {
		s.logger.Info("compaction: skipped",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Any("reason", err),
		)
		return compaction.TriggerConfig{}, nil
	}
	if err != nil {
		return compaction.TriggerConfig{}, err
	}
	cfg.BotID = req.BotID
	cfg.SessionID = req.ThreadID
	cfg.Ratio = ratio
	cfg.TotalInputTokens = inputTokens
	cfg.HTTPClient = s.streamHTTPClient
	return cfg, nil
}

// syncCompactionTargetTokens derives the synchronous-compaction goal from the
// context budget: after compaction the kept tail should be the (100-ratio)%
// share the user asked to preserve, instead of a fixed absolute size.
func syncCompactionTargetTokens(contextTokenBudget, ratio int) int {
	if contextTokenBudget <= 0 || ratio >= 100 {
		return 0
	}
	return contextTokenBudget * (100 - ratio) / 100
}
