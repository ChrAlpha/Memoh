package application

import (
	"context"
	"fmt"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func (s *Service) chatBudgetObserver(req ChatRequest, layout chatHistoryLayout) func(context.Context, native.RunConfig) {
	return func(ctx context.Context, cfg native.RunConfig) {
		budget, pressure := layout.availableHistoryBudget(cfg)
		if budget > 0 {
			s.scheduleAllowanceCompaction(ctx, req, pressure, budget, cfg.CurrentModelUUID)
		}
	}
}

type chatHistoryLayout struct {
	forkCount      int
	historyCount   int
	pressureTokens int
}

func (l chatHistoryLayout) owns(frag contextfrag.ContextFrag) bool {
	index := frag.Provenance.Index
	return frag.Slot == contextfrag.SlotHistory && index >= l.forkCount && index < l.historyCount &&
		frag.ID == fmt.Sprintf("message.%03d", index)
}

func (l chatHistoryLayout) availableHistoryBudget(cfg native.RunConfig) (int, int) {
	available, history := historyAllowance(cfg, l.owns)
	pressure := max(contextfrag.ProviderBudgetTokensFromBytes(int(contextfrag.BudgetBytesForTokens(l.pressureTokens))), history)
	if pressure <= available {
		return 0, pressure
	}
	return max(0, available), pressure
}

// historyAllowance returns what the final history budget leaves after every
// frag that is neither system, current input nor history, and the history's
// cost.
func historyAllowance(cfg native.RunConfig, history func(contextfrag.ContextFrag) bool) (int, int) {
	plan := cfg.ContextManifest.BudgetPlan
	if plan == nil {
		return 0, 0
	}
	available, cost := plan.HistoryBudget, 0
	for _, frag := range cfg.ContextSourceFrags {
		switch {
		case frag.Slot == contextfrag.SlotSystem, frag.Slot == contextfrag.SlotCurrentUser, frag.Kind == contextfrag.KindCurrentUserMessage:
		case history(frag):
			cost += contextfrag.ResolveProviderBudgetFragTokens(frag)
		default:
			available -= contextfrag.ResolveProviderBudgetFragTokens(frag)
		}
	}
	return available, cost
}
