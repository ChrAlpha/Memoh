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

// mustKeepProviderSystemFrag keeps a fragment's own cap from dropping a
// system section on its own authority. Only the system budget, which prices
// the whole system prompt against its share of the window, drops one, and
// only as far as its retention tier allows.
func mustKeepProviderSystemFrag(frag contextfrag.ContextFrag) bool {
	return frag.Slot == contextfrag.SlotSystem
}

// Select keeps every history fragment: a request that does not fit its
// budget is compacted or fails, never trimmed. What it leaves out is what no
// budget keeps: fragments below their slot's trust floor, losers of a
// conflict group, fragments over their own cap, system sections over the
// system share of the window, and a tool exchange missing its other half.
func (*FragmentSelector) Select(frags []contextfrag.ContextFrag, profile IntentProfile, budget BudgetEnvelope) SelectionResult {
	frags, gated := applyTrustGate(frags, profile)
	frags, superseded := resolveConflictGroups(frags)
	frags, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings := enforceFragBudgets(
		frags,
		profile,
		systemBudgetPlanActive(profile, budget.Plan),
	)
	frags, systemBudgetDropped, fatalError := enforceSystemBudget(frags, profile, budget.Plan, fragBudgetDropped)
	// A view that cannot render keeps what is left whole for its audit.
	var orphans map[int]string
	if fatalError == nil {
		orphans = orphanExchangeDrops(tagFragments(frags, profile))
	}
	result := splitSelection(frags, orphans)
	result.FatalError = fatalError
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

// splitSelection keeps every fragment but the dropped ones, each recorded
// with its reason.
func splitSelection(frags []contextfrag.ContextFrag, drops map[int]string) SelectionResult {
	result := SelectionResult{Selected: make([]contextfrag.ContextFrag, 0, len(frags)-len(drops))}
	for i, frag := range frags {
		if reason, dropped := drops[i]; dropped {
			result.recordDrop(frag, reason)
			continue
		}
		result.Selected = append(result.Selected, frag)
	}
	return result
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
