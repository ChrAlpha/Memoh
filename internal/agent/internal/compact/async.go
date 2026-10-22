package compact

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/job"
	"github.com/felinics/memoh/internal/settings"
)

// asyncInputTokens is the pressure the background trigger judges a
// finished run by: the run's own estimate when it kept one.
func asyncInputTokens(rc runreq.ResolvedContext, providerInputTokens int) int {
	if rc.Pressure != nil {
		return rc.Pressure.Tokens()
	}
	return providerInputTokens
}

// RunCommitted decides on the caller's goroutine whether this turn's pressure
// calls for compaction, so a turn below the threshold starts no unit, and runs
// the compaction as a background unit when it does.
func (c *Compactor) RunCommitted(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int) {
	plan, ok := c.planAsync(ctx, req, rc, inputTokens)
	if !ok {
		return
	}
	job.Go(ctx, c.logger, "agent.compaction", job.Options{}, func(ctx context.Context) error {
		return c.runAsync(ctx, plan)
	},
		slog.String("bot_id", req.BotID),
		slog.String("session_id", req.ThreadID),
		slog.Int("input_tokens", plan.inputTokens),
		slog.Int("threshold", plan.threshold),
	)
}

// asyncPlan is what an automatic compaction trigger runs with.
type asyncPlan struct {
	req         runreq.ChatRequest
	rc          runreq.ResolvedContext
	settings    settings.Settings
	inputTokens int
	threshold   int
}

// planAsync reports whether the pressure crosses the automatic trigger.
func (c *Compactor) planAsync(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, inputTokens int) (asyncPlan, bool) {
	inputTokens = asyncInputTokens(rc, inputTokens)
	if !c.runner.enabled() {
		c.logger.InfoContext(ctx, "compaction: skipped, service or settings nil")
		return asyncPlan{}, false
	}
	botSettings, err := c.settings.GetBot(ctx, req.BotID)
	if err != nil {
		result := errlog.Event(ctx, "agent.compaction", errs.Wrap(err, "load compaction settings", slog.String("bot_id", req.BotID)), errlog.Options{})
		c.logger.LogAttrs(ctx, result.Level, "compaction: failed to load settings", result.Attrs()...)
		return asyncPlan{}, false
	}
	threshold := AutoThreshold(botSettings.CompactionThreshold, rc.ContextTokenBudget)
	if threshold <= 0 {
		c.logger.InfoContext(ctx, "compaction: skipped, no usable threshold",
			slog.Int("configured_threshold", botSettings.CompactionThreshold),
			slog.Int("context_token_budget", rc.ContextTokenBudget),
		)
		return asyncPlan{}, false
	}
	if urgencyOf(inputTokens, rc.ContextTokenBudget, botSettings.CompactionThreshold) == urgencyNone {
		c.logger.InfoContext(ctx, "compaction: skipped, below threshold",
			slog.Int("input_tokens", inputTokens),
			slog.Int("threshold", threshold),
		)
		return asyncPlan{}, false
	}
	return asyncPlan{req: req, rc: rc, settings: botSettings, inputTokens: inputTokens, threshold: threshold}, true
}

// runAsync is the body of one automatic compaction unit.
func (c *Compactor) runAsync(ctx context.Context, plan asyncPlan) error {
	cfg, err := c.buildConfig(ctx, plan.req, plan.settings, plan.inputTokens, plan.rc.Model.ID)
	if err != nil {
		return errs.Wrap(err, "build compaction config")
	}
	if cfg.ModelID == "" {
		// buildConfig returns an empty cfg when no compaction model
		// is configured or the configured one is disabled. Skip the trigger
		// so the compaction service doesn't run hooks + fail on empty UUIDs.
		job.Annotate(ctx, slog.String("skipped", "no_compaction_model"))
		return nil
	}
	cfg.TargetTokens = targetTokens(plan.settings.CompactionTargetPercent, plan.rc.ContextTokenBudget)
	cfg.FixedTokens = c.runEndFixedTokens(ctx, plan)
	cfg.AllowFrontierFusion = true
	cfg.ContextWindowTokens = plan.rc.ContextTokenBudget
	cfg.HardPressure = syncShouldRun(plan.inputTokens, plan.rc.ContextTokenBudget)
	return c.drainBacklog(ctx, cfg)
}

// runEndFixedTokens prices what the run's end request holds outside the
// history rows. The judged pressure of a run is its whole request, so the
// rows are measured from the session as it now stands. A replacement's cutoff
// is dropped: the replaced turn is hidden once the run has finalized. A
// discuss turn judges raw history alone, so it has no fixed part.
func (c *Compactor) runEndFixedTokens(ctx context.Context, plan asyncPlan) int {
	if plan.rc.Pressure == nil {
		return 0
	}
	req := plan.req
	req.HistoryCutoffBeforeMessageID = ""
	req.RequiredHistoryMessageID = ""
	req.PersistedUserMessageID = ""
	req.UserMessagePersisted = false
	assembled, err := c.transcript.Assemble(ctx, transcript.ForChat(transcript.PurposeContinuation, req, plan.rc.ContextTokenBudget))
	if err != nil {
		c.logger.WarnContext(ctx, "compaction: run-end fixed tokens unavailable, using none",
			slog.String("bot_id", req.BotID), slog.String("session_id", req.ThreadID), slog.Any("error", err))
		return 0
	}
	return fixedRequestTokens(plan.inputTokens, assembled.Records)
}

// drainBacklog runs bounded consecutive passes until the backlog is
// drained (noop), a pass fails, or the pass cap is reached, so a small
// summarizer window cannot strand a large backlog for future turns. The
// session barrier is re-acquired per pass: a turn that arrives between
// passes runs before the next summarizer call instead of waiting out the
// whole drain.
func (c *Compactor) drainBacklog(ctx context.Context, cfg compaction.TriggerConfig) error {
	for pass := 0; pass < maxAsyncPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := c.runPass(ctx, cfg)
		if err != nil {
			return err
		}
		if res.Status != compaction.StatusOK {
			c.logger.InfoContext(ctx, "compaction: backlog pass ended without a summary",
				slog.String("bot_id", cfg.BotID),
				slog.String("session_id", cfg.SessionID),
				slog.String("status", res.Status),
				slog.String("reason", res.Reason))
			return nil
		}
	}
	return nil
}

func (c *Compactor) runPass(ctx context.Context, cfg compaction.TriggerConfig) (compaction.Result, error) {
	done := c.enter(cfg.BotID, cfg.SessionID)
	defer done()
	return c.runner.RunCompactionSync(ctx, cfg)
}
