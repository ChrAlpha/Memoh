package contextview

import (
	"sort"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

const (
	// DefaultOutputReserveTokens is the conservative completion allowance used
	// until model configuration exposes an explicit maximum output size.
	DefaultOutputReserveTokens = 8192
	// MinimumSystemBudgetTokens prevents an active plan from treating a tiny
	// positive remainder as a usable system envelope.
	MinimumSystemBudgetTokens = 256

	systemBudgetDropReason = "system_budget"
	systemBudgetMarkerID   = "system.budget_notice"
)

// ComputeContextBudgetPlan allocates the fixed reserves named by the provider
// envelope contract. Passing outputReserve explicitly leaves the source seam
// ready for a future model-level maximum without inventing one on RunConfig.
func ComputeContextBudgetPlan(window, outputReserve, toolDefsCost, currentRequestCost int) (*contextfrag.ContextBudgetPlan, error) {
	if window == 0 {
		return nil, nil
	}
	plan := &contextfrag.ContextBudgetPlan{
		Window:             window,
		OutputReserve:      outputReserve,
		ToolDefsCost:       toolDefsCost,
		CurrentRequestCost: currentRequestCost,
	}
	remaining := window - outputReserve - toolDefsCost - currentRequestCost
	if window < 0 || outputReserve < 0 || toolDefsCost < 0 || currentRequestCost < 0 ||
		remaining < MinimumSystemBudgetTokens {
		plan.SystemBudget = MinimumSystemBudgetTokens
		return plan, contextfrag.ErrBudgetUnsatisfied
	}
	plan.SystemBudget = remaining
	return plan, nil
}

type systemBudgetCandidate struct {
	index int
	frag  contextfrag.ContextFrag
}

func enforceSystemBudget(
	frags []contextfrag.ContextFrag,
	profile IntentProfile,
	plan *contextfrag.ContextBudgetPlan,
) ([]contextfrag.ContextFrag, []contextfrag.ContextFrag, error) {
	if plan == nil ||
		(profile.Intent != contextfrag.IntentRunConfigPreProvider && profile.Intent != contextfrag.IntentDiscussReply) {
		return frags, nil, nil
	}

	total := systemFragCost(frags)
	if total <= plan.SystemBudget {
		finishSystemBudgetPlan(plan, total)
		return frags, nil, nil
	}

	candidates := make([]systemBudgetCandidate, 0)
	for i, frag := range frags {
		if frag.Slot != contextfrag.SlotSystem ||
			frag.Budget.Overflow == contextfrag.OverflowKeep ||
			(frag.RetentionTier != contextfrag.RetentionOptional && frag.RetentionTier != contextfrag.RetentionPreferred) {
			continue
		}
		candidates = append(candidates, systemBudgetCandidate{index: i, frag: frag})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i].frag, candidates[j].frag
		if left.RetentionTier != right.RetentionTier {
			return left.RetentionTier == contextfrag.RetentionOptional
		}
		if left.DropPriority != right.DropPriority {
			return left.DropPriority.DropsBefore(right.DropPriority)
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return left.ID < right.ID
	})

	droppedIndexes := make(map[int]bool, len(candidates))
	dropped := make([]contextfrag.ContextFrag, 0, len(candidates))
	droppedIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		droppedIndexes[candidate.index] = true
		dropped = append(dropped, candidate.frag)
		droppedIDs = append(droppedIDs, candidate.frag.ID)
		marker := systemBudgetMarkerFrag(droppedIDs, firstSystemScope(frags))
		selected := systemBudgetSelection(frags, droppedIndexes, marker)
		actual := systemFragCost(selected)
		if actual <= plan.SystemBudget {
			finishSystemBudgetPlan(plan, actual)
			return selected, dropped, nil
		}
	}

	var marker contextfrag.ContextFrag
	if len(droppedIDs) > 0 {
		marker = systemBudgetMarkerFrag(droppedIDs, firstSystemScope(frags))
	}
	selected := systemBudgetSelection(frags, droppedIndexes, marker)
	actual := systemFragCost(selected)
	finishSystemBudgetPlan(plan, actual)
	return selected, dropped, contextfrag.ErrProtectedContextOverflow
}

func finishSystemBudgetPlan(plan *contextfrag.ContextBudgetPlan, actual int) {
	plan.ActualSystemCost = actual
	plan.HistoryBudget = plan.SystemBudget - actual
	if plan.HistoryBudget < 1 {
		plan.HistoryBudget = 1
	}
}

func systemFragCost(frags []contextfrag.ContextFrag) int {
	total := 0
	count := 0
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem {
			total += contextfrag.ResolveFragTokens(frag)
			count++
		}
	}
	// SDK rendering inserts "\n\n" between every pair of system fragments,
	// including empty fragments. Count each non-empty boundary as one token so
	// per-fragment integer flooring can never make the rendered system prompt
	// look cheaper than its section structure.
	if count > 1 {
		total += count - 1
	}
	return total
}

func firstSystemScope(frags []contextfrag.ContextFrag) contextfrag.Scope {
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem {
			return frag.Scope
		}
	}
	return contextfrag.Scope{}
}

func systemBudgetSelection(
	frags []contextfrag.ContextFrag,
	droppedIndexes map[int]bool,
	marker contextfrag.ContextFrag,
) []contextfrag.ContextFrag {
	kept := make([]contextfrag.ContextFrag, 0, len(frags)-len(droppedIndexes)+1)
	lastSystem := -1
	for i, frag := range frags {
		if droppedIndexes[i] {
			continue
		}
		kept = append(kept, frag)
		if frag.Slot == contextfrag.SlotSystem {
			lastSystem = len(kept) - 1
		}
	}
	if marker.ID == "" {
		return kept
	}
	insertAt := lastSystem + 1
	kept = append(kept, contextfrag.ContextFrag{})
	copy(kept[insertAt+1:], kept[insertAt:])
	kept[insertAt] = marker
	return kept
}

func systemBudgetMarkerFrag(droppedIDs []string, scope contextfrag.Scope) contextfrag.ContextFrag {
	text := "[System Notice] Some system sections were omitted to fit the context window: " +
		strings.Join(droppedIDs, ", ") + "."
	frag := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:            systemBudgetMarkerID,
		Kind:          contextfrag.KindSystemPolicy,
		Role:          sdk.MessageRoleSystem,
		Slot:          contextfrag.SlotSystem,
		Text:          text,
		Priority:      int(^uint(0) >> 1),
		RetentionTier: contextfrag.RetentionRequired,
		CacheClass:    contextfrag.CacheDynamic,
		Trust:         contextfrag.TrustSystem,
		Scope:         scope,
		Source:        contextfrag.SourceRunConfig,
		Collector:     "system_budget",
		Render:        contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		Budget:        contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
	return contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{frag})[0]
}

func appendSystemBudgetDrops(result SelectionResult, dropped []contextfrag.ContextFrag) SelectionResult {
	for _, frag := range dropped {
		result.Dropped = append(result.Dropped, frag)
		result.Summary.DropReasons = append(result.Summary.DropReasons, DropRecord{
			FragID: frag.ID,
			Ref:    frag.Ref,
			Reason: systemBudgetDropReason,
		})
	}
	result.Summary.TotalCollected += len(dropped)
	result.Summary.TotalDropped += len(dropped)
	return result
}
