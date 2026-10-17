package application

import (
	"context"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/job"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/oauthctx"
	"github.com/felinics/memoh/internal/providers"
	"github.com/felinics/memoh/internal/settings"
)

// Automatic compaction derives its soft trigger, hard backstop, and target
// from the chat model's context window. A configured absolute threshold only
// overrides the async trigger and is capped at the hard backstop.
const (
	compactionSoftThresholdPercent = 50
	compactionHardThresholdPercent = 75
	defaultCompactionTargetPercent = 40
	// maxAsyncCompactionPasses bounds how many consecutive summarizer calls
	// one background trigger may spend draining a backlog.
	maxAsyncCompactionPasses = 3
)

// AutoCompactionThreshold is the async trigger level, exported so read-side
// surfaces label the same level the turn path acts on. A zero return leaves
// automatic compaction off when no usable model window is available. The
// synchronous backstop is deliberately not exposed: a reader cannot observe it.
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

// syncBackstopTargetTokens caps the hard-share backstop at the soft share so it
// always makes progress and lands below the soft trigger, restoring hysteresis.
func syncBackstopTargetTokens(targetPercent *int, contextTokenBudget int) int {
	softTarget := max(1, contextTokenBudget*compactionSoftThresholdPercent/100)
	return min(compactionTargetTokens(targetPercent, contextTokenBudget), softTarget)
}

func syncCompactionShouldRun(pressure, contextTokenBudget int) bool {
	threshold := hardCompactionThreshold(contextTokenBudget)
	return threshold > 0 && pressure >= threshold
}

// compactionLevel is how urgently a context needs compacting.
type compactionLevel int

const (
	compactionNone compactionLevel = iota
	// compactionAsync starts a background compaction; the turn proceeds.
	compactionAsync
	// compactionSync blocks the turn until compaction has run.
	compactionSync
)

// compactionNeed is the one place that turns context pressure into a
// compaction decision. userThreshold, when positive, replaces the soft share
// that triggers the background run; it never exceeds the hard share.
func compactionNeed(pressure, contextTokenBudget, userThreshold int) compactionLevel {
	switch {
	case syncCompactionShouldRun(pressure, contextTokenBudget):
		return compactionSync
	case compaction.ShouldCompact(pressure, AutoCompactionThreshold(userThreshold, contextTokenBudget)):
		return compactionAsync
	default:
		return compactionNone
	}
}

// compactableRequestTokens prices the raw history rows a compaction could
// claim, in the units of the request pressure they are judged against.
// Active summaries are not compactable and are left out.
func compactableRequestTokens(records []historyfrag.HistoryRecord) int {
	rows := make([]historyfrag.HistoryRecord, 0, len(records))
	for _, record := range records {
		if record.Kind == contextfrag.KindConversationSummary || record.Lifecycle == historyfrag.LifecycleActiveSummary {
			continue
		}
		rows = append(rows, record)
	}
	return runreq.RequestInputTokens(0, stepMessages(rows))
}

// fixedRequestTokens is the part of a judged request that no compaction can
// shrink: the pressure less the rows it was measured from. It holds the system
// prompt and tool definitions, this request's own messages, and the summaries.
func fixedRequestTokens(pressure int, records []historyfrag.HistoryRecord) int {
	return max(0, pressure-compactableRequestTokens(records))
}

// lastRunFixedTokens is what the session's latest run spent on its system
// prompt and tool definitions, read from its context lifecycle. A turn judges
// its pressure before either is built, so the previous run's measurement
// stands in for them; a session that has none has no history to compact
// either.
func (s *Service) lastRunFixedTokens(ctx context.Context, sessionID string) int {
	if s == nil || s.queries == nil {
		return 0
	}
	parsed, err := db.ParseUUID(sessionID)
	if err != nil {
		return 0
	}
	raw, err := s.queries.GetLatestContextLifecycleBySession(ctx, parsed)
	if err != nil {
		return 0
	}
	snapshot, err := contextfrag.DecodeLifecycleSnapshot(raw)
	if err != nil {
		return 0
	}
	return runreq.LifecycleFixedTokens(snapshot)
}

// turnRequestEstimate prices the request a turn is about to make with
// whatever history it ends up assembled from. Everything but the history is
// fixed: the system prompt and tools, and this turn's own messages (fork
// context, notices, recalled memory, requested skills, the user's message).
type turnRequestEstimate struct {
	service   *Service
	ctx       context.Context
	sessionID string
	// own is this turn's messages that no history row holds.
	own []sdk.Message
	// systemAndTools is read on first use: a turn with nothing to compact
	// never needs it.
	systemAndTools      int
	systemAndToolsKnown bool
}

func (e *turnRequestEstimate) tokens(history []runreq.ModelMessage) int {
	if !e.systemAndToolsKnown {
		e.systemAndTools = e.service.lastRunFixedTokens(e.ctx, e.sessionID)
		e.systemAndToolsKnown = true
	}
	return runreq.RequestInputTokens(e.systemAndTools, e.conversation(history))
}

// conversation is the turn's messages with history.
func (e *turnRequestEstimate) conversation(history []runreq.ModelMessage) []sdk.Message {
	return append(runreq.ModelMessagesToSDKMessages(history), e.own...)
}

// currentUserMessageEstimate stands in for the user's message when it is not
// a stored row yet: its text and one image part per image attachment, which is
// how the estimator prices them.
func currentUserMessageEstimate(req runreq.ChatRequest, modelQuery string) []sdk.Message {
	if req.ReusePersistedUserMessage {
		return nil
	}
	parts := []sdk.MessagePart{sdk.TextPart{Text: modelQuery}}
	for _, attachment := range req.Attachments {
		if strings.EqualFold(strings.TrimSpace(attachment.Type), "image") {
			parts = append(parts, sdk.ImagePart{})
		}
	}
	return []sdk.Message{{Role: sdk.MessageRoleUser, Content: parts}}
}

// asyncCompactionInputTokens is the pressure the background trigger judges a
// finished run by: the run's own estimate when it kept one.
func asyncCompactionInputTokens(rc runreq.ResolvedContext, providerInputTokens int) int {
	if rc.Pressure != nil {
		return rc.Pressure.Tokens()
	}
	return providerInputTokens
}

// maybeCompact decides on the caller's goroutine whether this turn's pressure
// calls for compaction, so a turn below the threshold starts no unit, and runs
// the compaction as a background unit when it does.
func (s *Service) maybeCompact(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int) {
	plan, ok := s.planCompaction(ctx, req, rc, inputTokens)
	if !ok {
		return
	}
	job.Go(ctx, s.logger, "agent.compaction", job.Options{}, func(ctx context.Context) error {
		return s.runCompaction(ctx, plan)
	},
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", plan.inputTokens),
		slog.Int("threshold", plan.threshold),
	)
}

// compactionPlan is what an automatic compaction trigger runs with.
type compactionPlan struct {
	req         runreq.ChatRequest
	rc          runreq.ResolvedContext
	settings    settings.Settings
	inputTokens int
	threshold   int
}

// planCompaction reports whether the pressure crosses the automatic trigger.
func (s *Service) planCompaction(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int) (compactionPlan, bool) {
	inputTokens = asyncCompactionInputTokens(rc, inputTokens)
	if s.compactionService == nil || s.settingsService == nil {
		s.logger.InfoContext(ctx, "compaction: skipped, service or settings nil")
		return compactionPlan{}, false
	}
	botSettings, err := s.settingsService.GetBot(ctx, req.BotID)
	if err != nil {
		result := errlog.Event(ctx, "agent.compaction", errs.Wrap(err, "load compaction settings", slog.String("bot_id", req.BotID)), errlog.Options{})
		s.logger.LogAttrs(ctx, result.Level, "compaction: failed to load settings", result.Attrs()...)
		return compactionPlan{}, false
	}
	threshold := AutoCompactionThreshold(botSettings.CompactionThreshold, rc.ContextTokenBudget)
	if threshold <= 0 {
		s.logger.InfoContext(ctx, "compaction: skipped, no usable threshold",
			slog.Int("configured_threshold", botSettings.CompactionThreshold),
			slog.Int("context_token_budget", rc.ContextTokenBudget),
		)
		return compactionPlan{}, false
	}
	if compactionNeed(inputTokens, rc.ContextTokenBudget, botSettings.CompactionThreshold) == compactionNone {
		s.logger.InfoContext(ctx, "compaction: skipped, below threshold",
			slog.Int("input_tokens", inputTokens),
			slog.Int("threshold", threshold),
		)
		return compactionPlan{}, false
	}
	return compactionPlan{req: req, rc: rc, settings: botSettings, inputTokens: inputTokens, threshold: threshold}, true
}

// runCompaction is the body of one automatic compaction unit.
func (s *Service) runCompaction(ctx context.Context, plan compactionPlan) error {
	cfg, err := s.buildCompactionConfig(ctx, plan.req, plan.settings, plan.inputTokens, plan.rc.Model.ID)
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
	cfg.TargetTokens = compactionTargetTokens(plan.settings.CompactionTargetPercent, plan.rc.ContextTokenBudget)
	cfg.FixedTokens = s.runEndFixedTokens(ctx, plan)
	cfg.AllowFrontierFusion = true
	cfg.ContextWindowTokens = plan.rc.ContextTokenBudget
	cfg.HardPressure = syncCompactionShouldRun(plan.inputTokens, plan.rc.ContextTokenBudget)
	return s.drainCompactionBacklog(ctx, cfg)
}

// runEndFixedTokens prices what the run's end request holds outside the
// history rows. The judged pressure of a run is its whole request, so the
// rows are measured from the session as it now stands. A replacement's cutoff
// is dropped: the replaced turn is hidden once the run has finalized. A
// discuss turn judges raw history alone, so it has no fixed part.
func (s *Service) runEndFixedTokens(ctx context.Context, plan compactionPlan) int {
	if plan.rc.Pressure == nil {
		return 0
	}
	req := plan.req
	req.HistoryCutoffBeforeMessageID = ""
	req.RequiredHistoryMessageID = ""
	req.PersistedUserMessageID = ""
	req.UserMessagePersisted = false
	assembled, err := s.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeContinuation, req, plan.rc.ContextTokenBudget))
	if err != nil {
		s.logger.WarnContext(ctx, "compaction: run-end fixed tokens unavailable, using none",
			slog.String("bot_id", req.BotID), slog.String("session_id", req.ThreadID), slog.Any("error", err))
		return 0
	}
	return fixedRequestTokens(plan.inputTokens, assembled.Records)
}

// drainCompactionBacklog runs bounded consecutive passes until the backlog is
// drained (noop), a pass fails, or the pass cap is reached, so a small
// summarizer window cannot strand a large backlog for future turns. The
// session barrier is re-acquired per pass: a turn that arrives between
// passes runs before the next summarizer call instead of waiting out the
// whole drain.
func (s *Service) drainCompactionBacklog(ctx context.Context, cfg compaction.TriggerConfig) error {
	for pass := 0; pass < maxAsyncCompactionPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.runCompactionPass(ctx, cfg)
		if err != nil {
			return err
		}
		if res.Status != compaction.StatusOK {
			s.logger.InfoContext(ctx, "compaction: backlog pass ended without a summary",
				slog.String("bot_id", cfg.BotID),
				slog.String("session_id", cfg.SessionID),
				slog.String("status", res.Status),
				slog.String("reason", res.Reason))
			return nil
		}
	}
	return nil
}

func (s *Service) runCompactionPass(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	done := s.enterSessionCompaction(cfg.BotID, cfg.SessionID)
	defer done()
	return s.compactionService.RunCompactionSync(ctx, cfg)
}

// runCompactionSyncFixed runs compaction synchronously when context reaches the
// blocking share of the model's context window and reports the session-scoped
// result. A compaction already in flight for the session is waited for and its
// outcome reused; a noop (failure cooldown or nothing to compact) means the
// caller proceeds with the context it has. The judged pressure includes
// fixedTokens that no compaction can shrink. The rows are aimed at the target
// less that part, so the target and the trigger measure the same request. A
// replacement request compacts only the history before its cutoff.
func (s *Service) runCompactionSyncFixed(ctx context.Context, req runreq.ChatRequest, inputTokens, fixedTokens, contextTokenBudget int, turnModelID string) compaction.Result {
	if s.compactionService == nil || s.settingsService == nil {
		s.logger.WarnContext(ctx, "compaction sync: skipped, service or settings nil")
		return compaction.Result{Status: compaction.StatusNoop, Reason: compaction.ReasonNoModel}
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
		// Same skip path as the async trigger above — no model or model
		// disabled means there is nothing to compact.
		return compaction.Result{Status: compaction.StatusNoop, Reason: compaction.ReasonNoModel}
	}
	cfg.TargetTokens = syncBackstopTargetTokens(botSettings.CompactionTargetPercent, contextTokenBudget)
	cfg.FixedTokens = fixedTokens
	cfg.ContextWindowTokens = contextTokenBudget
	cfg.HardPressure = syncCompactionShouldRun(inputTokens, contextTokenBudget)
	if cutoffID := strings.TrimSpace(req.HistoryCutoffBeforeMessageID); cutoffID != "" {
		// An unresolved cutoff admits nothing, so the compaction stands down
		// rather than summarizing rows after the replaced turn.
		boundary := s.transcript.LoadArtifactBoundary(ctx, nil, req.ThreadID, cutoffID)
		cfg.HistoryCutoff = &compaction.HistoryCutoff{BeforeMs: boundary.CutoffMs}
	}

	s.logger.InfoContext(ctx, "compaction sync: running synchronously",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", inputTokens),
		slog.Int("fixed_tokens", fixedTokens),
		slog.String("model_id", cfg.ModelID),
	)

	done := s.enterSessionCompactionForRun(req.BotID, req.ThreadID, strings.TrimSpace(req.RunID))
	defer done()
	res, err := s.compactionService.RunCompactionSync(ctx, cfg)
	if err != nil {
		s.logger.WarnContext(ctx, "compaction sync: failed", slog.Any("error", err))
		return compaction.Result{}
	}
	s.logger.InfoContext(ctx, "compaction sync: finished",
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.String("status", res.Status),
		slog.String("reason", res.Reason),
	)
	return res
}

// compactBeforeTurn is the pre-turn compaction backstop. A request at or
// over the hard share of the window, or a load that stopped short of the
// oldest row, is compacted before the model call and reassembled once from
// the summary. Compaction only ever removes raw rows, so a context without
// any is sent as it is. A compaction already running is waited for and the
// context is judged again, since it may have made this pass unnecessary.
// Failure, cooldown, or a context that still does not fit send the request as
// it is; the provider's own limit error is the outcome.
func (s *Service) compactBeforeTurn(ctx context.Context, req runreq.ChatRequest, turnModelID string, contextTokenBudget int, assembled transcript.Assembled, estimate *turnRequestEstimate) (transcript.Assembled, error) {
	if assembled.CompactableTokens == 0 {
		return assembled, nil
	}
	pressure := estimate.tokens(assembled.Messages)
	if !needsSyncCompaction(assembled, pressure, contextTokenBudget) {
		return assembled, nil
	}
	assembleAgain := func() (transcript.Assembled, error) {
		return s.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeRun, req, contextTokenBudget))
	}
	if s.awaitSessionCompaction(req.BotID, req.ThreadID, strings.TrimSpace(req.RunID)) {
		var err error
		if assembled, err = assembleAgain(); err != nil {
			return transcript.Assembled{}, err
		}
		pressure = estimate.tokens(assembled.Messages)
		if !needsSyncCompaction(assembled, pressure, contextTokenBudget) {
			return assembled, nil
		}
	}
	threshold := hardCompactionThreshold(contextTokenBudget)
	start := time.Now()
	res := s.runCompactionSyncFixed(ctx, req, pressure, fixedRequestTokens(pressure, assembled.Records), contextTokenBudget, turnModelID)
	s.logger.InfoContext(ctx, "sync_compaction_backstop",
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
	if pressure := estimate.tokens(compacted.Messages); needsSyncCompaction(compacted, pressure, contextTokenBudget) {
		s.logger.WarnContext(ctx, "sync_compaction_insufficient",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Int("pressure_tokens", pressure),
			slog.Int("threshold_tokens", threshold))
	}
	return compacted, nil
}

func needsSyncCompaction(assembled transcript.Assembled, pressure, contextTokenBudget int) bool {
	return assembled.Truncated || compactionNeed(pressure, contextTokenBudget, 0) == compactionSync
}

// buildCompactionConfig resolves the summarizer through the shared model
// policy — explicit override first, then the turn's actually-resolved model,
// then the bot chat default, then the session's latest model — and attaches
// credentials. Unavailable-model conditions stand the automatic trigger down
// silently; infrastructure errors propagate.
func (s *Service) buildCompactionConfig(ctx context.Context, req runreq.ChatRequest, botSettings settings.Settings, inputTokens int, turnModelID string) (compaction.TriggerConfig, error) {
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
