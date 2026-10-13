package contextview

import contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"

type FragmentSelector struct{}

func (*FragmentSelector) ProfileFor(intent contextfrag.Intent) IntentProfile {
	switch intent {
	case contextfrag.IntentRunConfigPreProvider, contextfrag.IntentDiscussReply:
		return IntentProfile{
			Intent:          intent,
			MustKeepSlots:   []contextfrag.Slot{contextfrag.SlotCurrentUser},
			MustKeepFrag:    mustKeepProviderSystemFrag,
			SlotTrustFloors: map[contextfrag.Slot]contextfrag.TrustLevel{contextfrag.SlotSystem: contextfrag.TrustWorkspace},
		}
	case contextfrag.IntentExternalAgentPrompt:
		// ACP system slots describe document position rather than instruction
		// authority, so external sections may legitimately occupy them and keep
		// their own per-section budgets.
		return IntentProfile{
			Intent:        intent,
			MustKeepSlots: []contextfrag.Slot{contextfrag.SlotCurrentUser},
		}
	default:
		return IntentProfile{Intent: intent}
	}
}

// mustKeepProviderSystemFrag keeps history-budget selection from claiming
// authority over system fragments. A later system-budget pass may apply the
// retention tier, but every system fragment surviving that pass remains
// protected here.
func mustKeepProviderSystemFrag(frag contextfrag.ContextFrag) bool {
	return frag.Slot == contextfrag.SlotSystem
}

func (*FragmentSelector) Select(frags []contextfrag.ContextFrag, profile IntentProfile, budget BudgetEnvelope) SelectionResult {
	frags, gated := applyTrustGate(frags, profile)
	frags, superseded := resolveConflictGroups(frags)
	frags, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings := enforceFragBudgets(
		frags,
		profile,
		systemBudgetPlanActive(profile, budget.Plan),
	)
	frags, systemBudgetDropped := enforceSystemBudget(frags, profile, budget.Plan, fragBudgetDropped)
	if budget.Plan != nil {
		budget.MaxTokens = budget.Plan.HistoryBudget
	}

	tagged := tagFragments(frags, profile)
	if budget.Plan != nil {
		for i := range tagged {
			tagged[i].Tokens = contextfrag.ResolveProviderBudgetFragTokens(tagged[i].Frag)
		}
		if protectedCost := protectedHistoryTokenCost(tagged); protectedCost > budget.MaxTokens {
			fragBudgetEdits = append(fragBudgetEdits, shrinkOversizedProtectedSummaries(tagged, protectedCost, budget.MaxTokens)...)
		}
	}

	selectedIndexes := allSelectedIndexes(tagged)
	var dropReasons map[int]string
	if budget.Plan != nil || budget.MaxTokens > 0 {
		var drops map[int]bool
		drops, dropReasons = orphanToolExchangeDrops(tagged)
		selectedIndexes = keptIndexes(tagged, drops)
	}
	result := selectionResultFromTaggedReasons(tagged, selectedIndexes, dropReasons)
	return finalizeSelection(
		result,
		profile,
		gated,
		superseded,
		fragBudgetDropped,
		fragBudgetEdits,
		fragBudgetWarnings,
		systemBudgetDropped,
	)
}

func finalizeSelection(
	result SelectionResult,
	profile IntentProfile,
	gated []contextfrag.ContextFrag,
	superseded []conflictLoser,
	fragBudgetDropped []fragBudgetDrop,
	fragBudgetEdits []fragBudgetEdit,
	fragBudgetWarnings []contextfrag.ValidationWarning,
	systemBudgetDropped []contextfrag.ContextFrag,
) SelectionResult {
	for _, edit := range fragBudgetEdits {
		result.Edited = append(result.Edited, edit.trace)
		if result.EditReasons == nil {
			result.EditReasons = make(map[string]string, len(fragBudgetEdits))
		}
		result.EditReasons[edit.fragID] = edit.reason
	}
	result.Warnings = append(result.Warnings, fragBudgetWarnings...)
	for _, frag := range gated {
		result.recordDrop(frag, "trust_gate:"+string(frag.Slot)+"_requires_"+string(profile.SlotTrustFloors[frag.Slot]))
	}
	for _, loser := range superseded {
		result.recordDrop(loser.frag, "precedence:superseded_by_"+loser.winnerID)
	}
	for _, dropped := range fragBudgetDropped {
		result.recordDrop(dropped.frag, dropped.reason)
	}
	for _, frag := range systemBudgetDropped {
		result.recordDrop(frag, systemBudgetDropReason)
	}
	result.Summary.TotalCollected = len(result.Selected) + len(result.Dropped)
	result.Summary.TotalSelected = len(result.Selected)
	result.Summary.TotalDropped = len(result.Dropped)
	return result
}

func (r *SelectionResult) recordDrop(frag contextfrag.ContextFrag, reason string) {
	r.Dropped = append(r.Dropped, frag)
	r.Summary.DropReasons = append(r.Summary.DropReasons, DropRecord{
		FragID: frag.ID,
		Ref:    frag.Ref,
		Reason: reason,
	})
}

func selectionResultFromTaggedReasons(
	tagged []TaggedFrag,
	selectedIndexes map[int]bool,
	reasonOverrides map[int]string,
) SelectionResult {
	selected := make([]contextfrag.ContextFrag, 0, len(selectedIndexes))
	dropped := make([]contextfrag.ContextFrag, 0, len(tagged)-len(selectedIndexes))
	dropRecords := make([]DropRecord, 0, len(tagged)-len(selectedIndexes))
	for i, taggedFrag := range tagged {
		if selectedIndexes[i] {
			selected = append(selected, taggedFrag.Frag)
			continue
		}
		reason := reasonOverrides[i]
		if reason == "" {
			reason = selectionDropReason(taggedFrag)
		}
		dropped = append(dropped, taggedFrag.Frag)
		dropRecords = append(dropRecords, DropRecord{
			FragID: taggedFrag.Frag.ID,
			Ref:    taggedFrag.Frag.Ref,
			Reason: reason,
		})
	}
	return SelectionResult{
		Selected: selected,
		Dropped:  dropped,
		Summary: SelectionSummary{
			TotalCollected: len(tagged),
			TotalSelected:  len(selected),
			TotalDropped:   len(dropped),
			DropReasons:    dropRecords,
		},
	}
}

func keptIndexes(tagged []TaggedFrag, drops map[int]bool) map[int]bool {
	kept := make(map[int]bool, len(tagged)-len(drops))
	for i := range tagged {
		if !drops[i] {
			kept[i] = true
		}
	}
	return kept
}

func allSelectedIndexes(tagged []TaggedFrag) map[int]bool {
	selected := make(map[int]bool, len(tagged))
	for i := range tagged {
		selected[i] = true
	}
	return selected
}

func applyTrustGate(frags []contextfrag.ContextFrag, profile IntentProfile) ([]contextfrag.ContextFrag, []contextfrag.ContextFrag) {
	if len(profile.SlotTrustFloors) == 0 {
		return frags, nil
	}
	kept := make([]contextfrag.ContextFrag, 0, len(frags))
	var dropped []contextfrag.ContextFrag
	for _, frag := range frags {
		floor, ok := profile.SlotTrustFloors[frag.Slot]
		if ok && contextfrag.TrustRank(frag.Trust) < contextfrag.TrustRank(floor) {
			dropped = append(dropped, frag)
			continue
		}
		kept = append(kept, frag)
	}
	return kept, dropped
}

type conflictLoser struct {
	frag     contextfrag.ContextFrag
	winnerID string
}

func resolveConflictGroups(frags []contextfrag.ContextFrag) ([]contextfrag.ContextFrag, []conflictLoser) {
	winners := make(map[string]int)
	for i, frag := range frags {
		if frag.ConflictKey == "" {
			continue
		}
		current, ok := winners[frag.ConflictKey]
		if !ok || conflictBeats(frag, frags[current]) {
			winners[frag.ConflictKey] = i
		}
	}
	if len(winners) == 0 {
		return frags, nil
	}
	kept := make([]contextfrag.ContextFrag, 0, len(frags))
	var losers []conflictLoser
	for i, frag := range frags {
		if frag.ConflictKey != "" && winners[frag.ConflictKey] != i {
			losers = append(losers, conflictLoser{frag: frag, winnerID: frags[winners[frag.ConflictKey]].ID})
			continue
		}
		kept = append(kept, frag)
	}
	return kept, losers
}

func conflictBeats(challenger, incumbent contextfrag.ContextFrag) bool {
	if challengerScope, incumbentScope := challenger.Scope.SpecificityRank(), incumbent.Scope.SpecificityRank(); challengerScope != incumbentScope {
		return challengerScope > incumbentScope
	}
	if challengerTrust, incumbentTrust := contextfrag.TrustRank(challenger.Trust), contextfrag.TrustRank(incumbent.Trust); challengerTrust != incumbentTrust {
		return challengerTrust > incumbentTrust
	}
	return true
}
