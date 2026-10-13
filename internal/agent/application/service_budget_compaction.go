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
			s.scheduleCompaction(ctx, req, pressure, budget, cfg.CurrentModelUUID)
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
	plan := cfg.ContextManifest.BudgetPlan
	if plan == nil {
		return 0, 0
	}
	available, raw, summaries := plan.HistoryBudget, 0, 0
	for _, frag := range cfg.ContextSourceFrags {
		if frag.Slot == contextfrag.SlotSystem || frag.Slot == contextfrag.SlotCurrentUser || frag.Kind == contextfrag.KindCurrentUserMessage {
			continue
		}
		if l.owns(frag) {
			if frag.Kind == contextfrag.KindConversationSummary {
				summaries += contextfrag.ResolveProviderBudgetFragTokens(frag)
			} else {
				raw += contextfrag.ResolveProviderBudgetFragTokens(frag)
			}
		} else {
			available -= contextfrag.ResolveProviderBudgetFragTokens(frag)
		}
	}
	pressure := max(contextfrag.ProviderBudgetTokensFromBytes(int(contextfrag.BudgetBytesForTokens(l.pressureTokens))), raw+summaries)
	if pressure <= available {
		return 0, pressure
	}
	return max(0, available), pressure
}
