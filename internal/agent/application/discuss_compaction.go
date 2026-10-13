package application

import (
	"context"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/turn"
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

func (s *Service) discussBudgetObserver(cmd turn.StartTurnCommand, modelID string) func(context.Context, native.RunConfig) {
	return func(ctx context.Context, cfg native.RunConfig) {
		plan := cfg.ContextManifest.BudgetPlan
		if plan == nil {
			return
		}
		available, pressure, newest := plan.HistoryBudget, 0, 0
		for _, message := range cmd.DiscussMessages {
			if message.Source != nil && message.Source.Current {
				newest = discussMessageTokens(message)
			}
		}
		for _, frag := range cfg.ContextSourceFrags {
			switch {
			case frag.Slot == contextfrag.SlotSystem, frag.Slot == contextfrag.SlotCurrentUser, frag.Kind == contextfrag.KindCurrentUserMessage:
			case frag.Kind == contextfrag.KindConversationSummary, frag.Slot == contextfrag.SlotHistory && frag.Kind == contextfrag.KindConversationEvent:
				pressure += contextfrag.ResolveProviderBudgetFragTokens(frag)
			default:
				available -= contextfrag.ResolveProviderBudgetFragTokens(frag)
			}
		}
		pressure = max(pressure, contextfrag.ProviderBudgetTokensFromBytes(int(contextfrag.BudgetBytesForTokens(max(0, cmd.DiscussContextTokens-newest)))))
		if available > 0 && pressure > available {
			s.scheduleCompaction(ctx, ChatRequest{BotID: cmd.BotID, ChatID: cmd.BotID, ThreadID: cmd.ThreadID, discussCurrentSources: cmd.DiscussCurrentSources}, pressure, available, modelID)
		}
	}
}
