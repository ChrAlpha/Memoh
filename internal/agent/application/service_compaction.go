package application

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/job"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/oauthctx"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
)

// Automatic compaction derives its trigger, hard-pressure level, and target
// from the chat model's context window. A configured absolute threshold only
// moves the trigger and is capped at the hard-pressure level.
const (
	compactionSoftThresholdPercent = 50
	compactionHardThresholdPercent = 75
	defaultCompactionTargetPercent = 40
	// maxCompactionPasses bounds how many consecutive summarizer calls one
	// trigger may spend draining a backlog.
	maxCompactionPasses = 3
)

// AutoCompactionThreshold is the trigger level, exported so read-side surfaces
// label the same level the turn path acts on. A zero return leaves automatic
// compaction off when no usable model window is available.
func AutoCompactionThreshold(userThreshold, contextTokenBudget int) int {
	if contextTokenBudget <= 0 {
		return 0
	}
	if userThreshold <= 0 {
		return max(1, contextTokenBudget*compactionSoftThresholdPercent/100)
	}
	return min(userThreshold, hardCompactionThreshold(contextTokenBudget))
}

func hardCompactionThreshold(contextTokenBudget int) int {
	if contextTokenBudget <= 0 {
		return 0
	}
	return max(1, contextTokenBudget*compactionHardThresholdPercent/100)
}

func compactionTargetTokens(targetPercent *int, contextTokenBudget int) int {
	if contextTokenBudget <= 0 {
		return 0
	}
	percent := defaultCompactionTargetPercent
	if targetPercent != nil && *targetPercent >= 1 && *targetPercent <= 99 {
		percent = *targetPercent
	}
	return max(1, contextTokenBudget*percent/100)
}

// hardPressureTargetTokens caps the target at the soft share so a pass under
// hard pressure always makes progress and lands below the soft trigger.
func hardPressureTargetTokens(targetPercent *int, contextTokenBudget int) int {
	softTarget := max(1, contextTokenBudget*compactionSoftThresholdPercent/100)
	return min(compactionTargetTokens(targetPercent, contextTokenBudget), softTarget)
}

func compactionUnderHardPressure(pressure, contextTokenBudget int) bool {
	threshold := hardCompactionThreshold(contextTokenBudget)
	return threshold > 0 && pressure >= threshold
}

// scheduleCompaction is the only automatic compaction path. It never delays
// the turn that measured the pressure: the turn keeps the context it already
// assembled, and the pass starts once the turn's run ends, so a summary that
// lands serves later turns.
func (s *Service) scheduleCompaction(ctx context.Context, req ChatRequest, pressure, contextTokenBudget int, turnModelID string) {
	if s.compactionService == nil || s.settingsService == nil || pressure <= 0 {
		return
	}
	AfterRunEnds(ctx, func() {
		ctx := context.WithoutCancel(ctx)
		plan, ok := s.planCompaction(ctx, req, pressure, contextTokenBudget, turnModelID)
		if !ok {
			return
		}
		_ = job.Run(ctx, s.logger, "agent.compaction", job.Options{}, func(ctx context.Context) error {
			return s.runCompaction(ctx, plan)
		},
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Int("input_tokens", pressure),
			slog.Int("threshold", plan.threshold),
		)
	})
}

type runEndKey struct{}

type runEnd struct {
	mu    sync.Mutex
	ended bool
	work  []func()
}

// WithRunEnd marks ctx as one run's and returns what the run calls when it
// ends. Work deferred to the end of the run then starts, so a summarizer call
// never competes with the run's own model calls and reads settings as they
// are then. The outermost mark owns the end: whoever writes the run's terminal
// state marks ctx first, and a nested mark ends nothing.
func WithRunEnd(ctx context.Context) (context.Context, func()) {
	if _, ok := ctx.Value(runEndKey{}).(*runEnd); ok {
		return ctx, func() {}
	}
	end := &runEnd{}
	return context.WithValue(ctx, runEndKey{}, end), func() {
		end.mu.Lock()
		work := end.work
		end.ended, end.work = true, nil
		end.mu.Unlock()
		for _, fn := range work {
			go fn()
		}
	}
}

// AfterRunEnds starts fn once the run ctx belongs to has ended, or at once
// when ctx belongs to no run.
func AfterRunEnds(ctx context.Context, fn func()) {
	if end, ok := ctx.Value(runEndKey{}).(*runEnd); ok {
		end.mu.Lock()
		if !end.ended {
			end.work = append(end.work, fn)
			end.mu.Unlock()
			return
		}
		end.mu.Unlock()
	}
	go fn()
}

// compactionPlan is what an automatic compaction trigger runs with.
type compactionPlan struct {
	req                ChatRequest
	settings           settings.Settings
	pressure           int
	threshold          int
	contextTokenBudget int
	turnModelID        string
}

// planCompaction reports whether the pressure crosses the automatic trigger.
func (s *Service) planCompaction(ctx context.Context, req ChatRequest, pressure, contextTokenBudget int, turnModelID string) (compactionPlan, bool) {
	botSettings, err := s.settingsService.GetBot(ctx, req.BotID)
	if err != nil {
		result := errlog.Event(ctx, "agent.compaction", errs.Wrap(err, "load compaction settings", slog.String("bot_id", req.BotID)), errlog.Options{})
		s.logger.LogAttrs(ctx, result.Level, "compaction: failed to load settings", result.Attrs()...)
		return compactionPlan{}, false
	}
	if !botSettings.CompactionEnabled {
		s.logger.InfoContext(ctx, "compaction: skipped, disabled")
		return compactionPlan{}, false
	}
	threshold := AutoCompactionThreshold(botSettings.CompactionThreshold, contextTokenBudget)
	if threshold <= 0 {
		s.logger.InfoContext(ctx, "compaction: skipped, no usable threshold",
			slog.Int("configured_threshold", botSettings.CompactionThreshold),
			slog.Int("context_token_budget", contextTokenBudget),
		)
		return compactionPlan{}, false
	}
	if !compaction.ShouldCompact(pressure, threshold) {
		s.logger.InfoContext(ctx, "compaction: skipped, below threshold",
			slog.Int("input_tokens", pressure),
			slog.Int("threshold", threshold),
		)
		return compactionPlan{}, false
	}
	return compactionPlan{req: req, settings: botSettings, pressure: pressure, threshold: threshold, contextTokenBudget: contextTokenBudget, turnModelID: turnModelID}, true
}

// runCompaction is the body of one automatic compaction unit.
func (s *Service) runCompaction(ctx context.Context, plan compactionPlan) error {
	cfg, err := s.buildCompactionConfig(ctx, plan.req, plan.settings, plan.pressure, plan.turnModelID)
	if err != nil {
		return errs.Wrap(err, "build compaction config")
	}
	if cfg.ModelID == "" {
		// buildCompactionConfig returns an empty cfg when no compaction model
		// is configured or the configured one is disabled. Skip the trigger
		// so the compaction service doesn't run hooks + fail on empty UUIDs.
		job.Annotate(ctx, slog.String("skipped", "no_compaction_model"))
		return nil
	}
	cfg.TargetTokens = compactionTargetTokens(plan.settings.CompactionTargetPercent, plan.contextTokenBudget)
	cfg.ProtectedSources = compactionProtectedSources(plan.req)
	cfg.AllowFrontierFusion = true
	cfg.ContextWindowTokens = plan.contextTokenBudget
	cfg.HardPressure = compactionUnderHardPressure(plan.pressure, plan.contextTokenBudget)
	if cfg.HardPressure {
		cfg.TargetTokens = hardPressureTargetTokens(plan.settings.CompactionTargetPercent, plan.contextTokenBudget)
	}
	return s.drainCompactionBacklog(ctx, cfg)
}

// drainCompactionBacklog runs bounded consecutive passes until the backlog is
// drained (noop), a pass fails, or the pass cap is reached, so a small
// summarizer window cannot strand a large backlog for future turns. A pass
// never selects the turns of the session's active run, so it needs no
// coordination with runs that start or finish around it.
func (s *Service) drainCompactionBacklog(ctx context.Context, cfg compaction.TriggerConfig) error {
	for pass := 0; pass < maxCompactionPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.sessionManager.AdmissionClosed() {
			// A pass claims its rows before the summarizer call; dying with the
			// process would keep them out of compaction until the claim is stale.
			return nil
		}
		res, err := s.compactionService.RunCompactionSync(ctx, cfg)
		if errors.Is(err, compaction.ErrIneffectiveSummary) {
			continue
		}
		if err != nil {
			return err
		}
		if res.Status != compaction.StatusOK && res.Status != compaction.StatusProgress && !res.ScanPending {
			return nil
		}
	}
	return nil
}

// runBudgetCompactionSync compacts while a turn waits, only when the turn's
// context does not fit its final history allowance: older history becomes
// summaries instead of being dropped. A noop leaves the turn to selection.
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
	if !botSettings.CompactionEnabled {
		s.logger.WarnContext(ctx, "compaction sync: compaction disabled, skipping")
		return compaction.Result{}
	}
	cfg, err := s.buildCompactionConfig(ctx, req, botSettings, inputTokens, turnModelID)
	if err != nil {
		s.logger.WarnContext(ctx, "compaction sync: failed to build config", slog.Any("error", err))
		return compaction.Result{}
	}
	if cfg.ModelID == "" {
		// No model or model disabled means there is nothing to compact.
		return compaction.Result{}
	}
	cfg.ProtectedSources = compactionProtectedSources(req)
	cfg.AllowFrontierFusion = true
	cfg.TargetTokens = hardPressureTargetTokens(botSettings.CompactionTargetPercent, historyBudget)
	cfg.ContextWindowTokens = historyBudget
	cfg.HardPressure = true
	cfg.HistoryBudgetTokens = historyBudget

	s.logger.InfoContext(ctx, "compaction sync: running synchronously",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", inputTokens),
		slog.String("model_id", cfg.ModelID),
	)

	res, err := s.compactionService.RunCompactionSync(ctx, cfg)
	if errors.Is(err, compaction.ErrIneffectiveSummary) || (err == nil && res.ScanPending) {
		res, err = s.compactionService.RunCompactionSync(ctx, cfg)
	}
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
// credentials. Unavailable-model conditions stand the automatic trigger down
// silently; infrastructure errors propagate.
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
// Discuss recovery passes the protected suffix its admission chose, so older
// unconsumed input stays compactable.
func compactionProtectedSources(req ChatRequest) []turn.ContextMessageSource {
	sources := append([]turn.ContextMessageSource(nil), req.discussCurrentSources...)
	if req.ExternalMessageID != "" {
		sources = append(sources, turn.ContextMessageSource{Kind: "external", ID: req.ExternalMessageID, Current: true})
	}
	if req.RequiredHistoryMessageID != "" {
		sources = append(sources, turn.ContextMessageSource{Kind: "history", ID: req.RequiredHistoryMessageID, Current: true})
	}
	return sources
}
