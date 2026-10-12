package application

import (
	"context"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/turn"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

func discussContextPressure(cmd turn.StartTurnCommand) int {
	total := 0
	for _, message := range cmd.DiscussMessages {
		total += discussMessageTokens(message)
	}
	return max(total, cmd.DiscussContextTokens)
}

// scheduleDiscussCompaction measures the composed discuss context against the
// resolved budget, or the absolute cap when the runtime carries no model
// window. What admission later drops is exactly what compaction must cover, so
// pressure is taken before admission. The pass keeps the unconsumed batch raw:
// Channel sends it again when the run fails.
func (s *Service) scheduleDiscussCompaction(ctx context.Context, cmd turn.StartTurnCommand, resolved ResolveRunConfigResult) {
	budget := resolved.ContextBudgetMaxTokens
	if budget <= 0 {
		budget = s.contextAbsoluteMaxTokens()
	}
	req := ChatRequest{BotID: cmd.BotID, ChatID: cmd.BotID, ThreadID: cmd.ThreadID, discussCurrentSources: cmd.DiscussCurrentSources}
	s.scheduleCompaction(ctx, req, discussContextPressure(cmd), budget, resolved.ModelID)
}

// recoverDiscussContext compacts while the turn waits, only when its context
// does not fit: older unconsumed input that no longer fits must be compacted
// before admission drops it. It reports whether the caller should recompose.
func (s *Service) recoverDiscussContext(ctx context.Context, cmd turn.StartTurnCommand, resolved ResolveRunConfigResult, runID string) bool {
	if ctx.Err() != nil || s.compactionService == nil || s.settingsService == nil {
		return false
	}
	pressure := discussContextPressure(cmd)
	budget := resolved.ContextBudgetMaxTokens
	if budget <= 0 {
		budget = s.contextAbsoluteMaxTokens()
	}
	_, admission := admitDiscussMessages(cmd.DiscussMessages, budget)
	if resolved.RuntimeType == sessionpkg.RuntimeACPAgent || sessionpkg.IsDirectRuntimeType(resolved.RuntimeType) {
		_, admission = admitDiscussAgentMessages(cmd.DiscussMessages, budget)
	}
	if !cmd.DiscussContextOverflow && !admission.ProtectedOverflow &&
		len(cmd.DiscussOmittedSources) == 0 && len(admission.OmittedSources) == 0 {
		return false
	}
	protected := admission.ProtectedSources
	if cmd.DiscussContextOverflow {
		// Channel rejected even the newest input beside the summaries, so
		// that newest input is the whole protected set.
		protected = nil
		if n := len(cmd.DiscussCurrentSources); n > 0 {
			protected = cmd.DiscussCurrentSources[n-1:]
		}
		budget = max(0, budget-cmd.DiscussCurrentTokens)
		if resolved.RuntimeType == sessionpkg.RuntimeACPAgent || sessionpkg.IsDirectRuntimeType(resolved.RuntimeType) {
			budget -= turn.EstimateTokensFromBytes(len(discussAgentPromptPrefix) + len(discussAgentPromptSuffix) + len("[assistant]\n\n\n") + turn.ContextBytesPerToken - 1)
		}
	} else {
		budget = admission.RecoveryBudgetTokens
	}
	if budget <= 0 {
		return false
	}
	start := time.Now()
	req := ChatRequest{BotID: cmd.BotID, ChatID: cmd.BotID, ThreadID: cmd.ThreadID, RunID: runID, discussCurrentSources: protected}
	res := s.runBudgetCompactionSync(ctx, req, pressure, budget, resolved.ModelID)
	s.logger.InfoContext(ctx, "discuss context recovery",
		slog.String("status", res.Status),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.String("bot_id", cmd.BotID),
		slog.String("session_id", cmd.ThreadID),
		slog.Int("pressure_tokens", pressure),
		slog.Int("budget_tokens", budget))
	return res.Status == compaction.StatusOK || res.Status == compaction.StatusProgress
}
