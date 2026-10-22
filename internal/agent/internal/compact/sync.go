package compact

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/oauthctx"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
)

// runSync runs compaction synchronously when context reaches the
// blocking share of the model's context window and reports the session-scoped
// result. A compaction already in flight for the session is waited for and its
// outcome reused; a noop (failure cooldown or nothing to compact) means the
// caller proceeds with the context it has. The judged pressure includes
// fixedTokens that no compaction can shrink. The rows are aimed at the target
// less that part, so the target and the trigger measure the same request. A
// replacement request compacts only the history before its cutoff.
func (c *Compactor) runSync(ctx context.Context, req runreq.ChatRequest, inputTokens, fixedTokens, contextTokenBudget int, turnModelID string) compaction.Result {
	if !c.runner.enabled() {
		c.logger.WarnContext(ctx, "compaction sync: skipped, service or settings nil")
		return compaction.Result{Status: compaction.StatusNoop, Reason: compaction.ReasonNoModel}
	}
	botSettings, err := c.settings.GetBot(ctx, req.BotID)
	if err != nil {
		c.logger.WarnContext(ctx, "compaction sync: failed to load settings", slog.Any("error", err))
		return compaction.Result{}
	}
	cfg, err := c.buildConfig(ctx, req, botSettings, inputTokens, turnModelID)
	if err != nil {
		c.logger.WarnContext(ctx, "compaction sync: failed to build config", slog.Any("error", err))
		return compaction.Result{}
	}
	if cfg.ModelID == "" {
		// Same skip path as the async trigger above — no model or model
		// disabled means there is nothing to compact.
		return compaction.Result{Status: compaction.StatusNoop, Reason: compaction.ReasonNoModel}
	}
	cfg.TargetTokens = syncBackstopTargetTokens(botSettings.CompactionTargetPercent, contextTokenBudget)
	cfg.FixedTokens = fixedTokens
	cfg.ContextWindowTokens = contextTokenBudget
	cfg.HardPressure = syncShouldRun(inputTokens, contextTokenBudget)
	if cutoffID := strings.TrimSpace(req.HistoryCutoffBeforeMessageID); cutoffID != "" {
		// An unresolved cutoff admits nothing, so the compaction stands down
		// rather than summarizing rows after the replaced turn.
		boundary := c.transcript.LoadArtifactBoundary(ctx, nil, req.ThreadID, cutoffID)
		cfg.HistoryCutoff = &compaction.HistoryCutoff{BeforeMs: boundary.CutoffMs}
	}

	c.logger.InfoContext(ctx, "compaction sync: running synchronously",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", inputTokens),
		slog.Int("fixed_tokens", fixedTokens),
		slog.String("model_id", cfg.ModelID),
	)

	done := c.enterForRun(req.BotID, req.ThreadID, strings.TrimSpace(req.RunID))
	defer done()
	res, err := c.runner.RunCompactionSync(ctx, cfg)
	if err != nil {
		c.logger.WarnContext(ctx, "compaction sync: failed", slog.Any("error", err))
		return compaction.Result{}
	}
	c.logger.InfoContext(ctx, "compaction sync: finished",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.String("status", res.Status),
		slog.String("reason", res.Reason),
	)
	return res
}

// BeforeTurn is the pre-turn compaction backstop. A request at or
// over the hard share of the window, or a load that stopped short of the
// oldest row, is compacted before the model call and reassembled once from
// the summary. Compaction only ever removes raw rows, so a context without
// any is sent as it is. A compaction already running is waited for and the
// context is judged again, since it may have made this pass unnecessary.
// Failure, cooldown, or a context that still does not fit send the request as
// it is; the provider's own limit error is the outcome. A turn cancelled
// during the pass ends with the cancellation's cause instead.
func (c *Compactor) BeforeTurn(ctx context.Context, req runreq.ChatRequest, turnModelID string, contextTokenBudget int, assembled transcript.Assembled, estimate *Estimate) (transcript.Assembled, error) {
	if assembled.CompactableTokens == 0 {
		return assembled, nil
	}
	pressure := estimate.Tokens(assembled.Messages)
	if !needsSync(assembled, pressure, contextTokenBudget) {
		return assembled, nil
	}
	assembleAgain := func() (transcript.Assembled, error) {
		return c.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeRun, req, contextTokenBudget))
	}
	if c.await(req.BotID, req.ThreadID, strings.TrimSpace(req.RunID)) {
		var err error
		if assembled, err = assembleAgain(); err != nil {
			return transcript.Assembled{}, err
		}
		pressure = estimate.Tokens(assembled.Messages)
		if !needsSync(assembled, pressure, contextTokenBudget) {
			return assembled, nil
		}
	}
	threshold := hardThreshold(contextTokenBudget)
	start := time.Now()
	res := c.runSync(ctx, req, pressure, fixedRequestTokens(pressure, assembled.Records), contextTokenBudget, turnModelID)
	if ctx.Err() != nil {
		// The turn was cancelled while its context was compacted. A failed pass
		// is otherwise sent on as it is, but nobody is waiting for this turn's
		// model call any more.
		return transcript.Assembled{}, context.Cause(ctx)
	}
	c.logger.InfoContext(ctx, "sync_compaction_backstop",
		slog.String("status", res.Status),
		slog.String("reason", res.Reason),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Bool("history_truncated", assembled.Truncated),
		slog.Int("pressure_tokens", pressure),
		slog.Int("threshold_tokens", threshold))
	if res.Status != compaction.StatusOK {
		return assembled, nil
	}
	compacted, err := assembleAgain()
	if err != nil {
		return transcript.Assembled{}, err
	}
	if pressure := estimate.Tokens(compacted.Messages); needsSync(compacted, pressure, contextTokenBudget) {
		c.logger.WarnContext(ctx, "sync_compaction_insufficient",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Int("pressure_tokens", pressure),
			slog.Int("threshold_tokens", threshold))
	}
	return compacted, nil
}

func needsSync(assembled transcript.Assembled, pressure, contextTokenBudget int) bool {
	return assembled.Truncated || urgencyOf(pressure, contextTokenBudget, 0) == urgencySync
}

// buildConfig resolves the summarizer through the shared model
// policy — explicit override first, then the turn's actually-resolved model,
// then the bot chat default, then the session's latest model — and attaches
// credentials. Unavailable-model conditions stand the automatic trigger down
// silently; infrastructure errors propagate.
func (c *Compactor) buildConfig(ctx context.Context, req runreq.ChatRequest, botSettings settings.Settings, inputTokens int, turnModelID string) (compaction.TriggerConfig, error) {
	sessionModelID := ""
	if strings.TrimSpace(botSettings.CompactionModelID) == "" && strings.TrimSpace(turnModelID) == "" && strings.TrimSpace(botSettings.ChatModelID) == "" {
		sessionModelID = models.LatestSessionModelID(ctx, c.queries, req.ThreadID)
	}
	resolution, err := models.ResolveCompactionModel(
		ctx,
		c.models,
		c.queries,
		botSettings.CompactionModelID,
		turnModelID,
		botSettings.ChatModelID,
		sessionModelID,
	)
	if models.IsCompactionModelUnavailable(err) {
		c.logger.InfoContext(ctx, "compaction: skipped",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Any("reason", err),
		)
		return compaction.TriggerConfig{}, nil
	}
	if err != nil {
		return compaction.TriggerConfig{}, err
	}
	authService := providers.NewService(nil, c.queries, "")
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
	cfg.HTTPClient = c.httpClient
	return cfg, nil
}
