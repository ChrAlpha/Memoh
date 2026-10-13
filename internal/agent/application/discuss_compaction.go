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
	s.scheduleDiscussCompactionWithin(ctx, cmd, s.discussBudget(resolved), resolved.ModelID)
}

// scheduleDiscussCompactionWithin reports the context's pressure against
// budget. A context that cannot run passes what its budget leaves beside the
// protected current input, so the pass shrinks history until that input fits.
func (s *Service) scheduleDiscussCompactionWithin(ctx context.Context, cmd turn.StartTurnCommand, budget int, modelID string) {
	if budget > 0 {
		s.scheduleCompaction(ctx, discussCompactionRequest(cmd), discussContextPressure(cmd), budget, modelID)
	}
}

func (s *Service) discussBudget(resolved ResolveRunConfigResult) int {
	if resolved.ContextBudgetMaxTokens > 0 {
		return resolved.ContextBudgetMaxTokens
	}
	return s.contextAbsoluteMaxTokens()
}

func discussCompactionRequest(cmd turn.StartTurnCommand) ChatRequest {
	return ChatRequest{BotID: cmd.BotID, ChatID: cmd.BotID, ThreadID: cmd.ThreadID, discussCurrentSources: cmd.DiscussCurrentSources}
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
			s.scheduleCompaction(ctx, discussCompactionRequest(cmd), pressure, available, modelID)
		}
	}
}
