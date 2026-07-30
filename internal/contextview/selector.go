package contextview

import contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"

type FragmentSelector struct{}

func (*FragmentSelector) ProfileFor(intent contextfrag.Intent) IntentProfile {
	switch intent {
	case contextfrag.IntentRunConfigPreProvider:
		return IntentProfile{
			Intent:          intent,
			MustKeepSlots:   []contextfrag.Slot{contextfrag.SlotCurrentUser},
			MustKeepFrag:    mustKeepProviderSystemFrag,
			SlotTrustFloors: map[contextfrag.Slot]contextfrag.TrustLevel{contextfrag.SlotSystem: contextfrag.TrustWorkspace},
		}
	case contextfrag.IntentCompactionCandidates:
		return IntentProfile{
			Intent:        intent,
			MustKeepSlots: []contextfrag.Slot{contextfrag.SlotSystem, contextfrag.SlotCurrentUser},
		}
	case contextfrag.IntentDiscussReply:
		return IntentProfile{
			Intent:          intent,
			MustKeepSlots:   []contextfrag.Slot{contextfrag.SlotCurrentUser},
			MustKeepFrag:    mustKeepProviderSystemFrag,
			SlotTrustFloors: map[contextfrag.Slot]contextfrag.TrustLevel{contextfrag.SlotSystem: contextfrag.TrustWorkspace},
		}
	case contextfrag.IntentACPRuntimePrompt:
		// The ACP prompt is one rendered document: its system slot marks
		// document position, not instruction authority, so external-trust
		// sections legitimately live there and their explicit budgets must
		// remain enforceable. The preamble declares OverflowKeep directly.
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
	var superseded []conflictLoser
	if isRetentionIntent(profile.Intent) {
		frags, superseded = resolveConflictGroups(frags)
	}
	var fragBudgetDropped []fragBudgetDrop
	var fragBudgetEdits []fragBudgetEdit
	var fragBudgetWarnings []contextfrag.ValidationWarning
	if isRetentionIntent(profile.Intent) {
		frags, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings = enforceFragBudgets(
			frags,
			profile,
			systemBudgetPlanActive(profile, budget.Plan),
		)
	}
	var systemBudgetDropped []contextfrag.ContextFrag
	var systemBudgetErr error
	frags, systemBudgetDropped, systemBudgetErr = enforceSystemBudget(frags, profile, budget.Plan, fragBudgetDropped)
	if systemBudgetErr != nil {
		tagged := tagFragments(frags, profile)
		result := selectionResultFromTagged(tagged, allSelectedIndexes(tagged))
		result = finishSelection(result, nil, nil, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings, gated, profile, superseded)
		result = appendSystemBudgetDrops(result, systemBudgetDropped)
		result.FatalError = systemBudgetErr
		return result
	}
	if budget.Plan != nil &&
		(profile.Intent == contextfrag.IntentRunConfigPreProvider || profile.Intent == contextfrag.IntentDiscussReply) {
		budget.MaxTokens = budget.Plan.HistoryBudget
	}
	var exchangeDropped []contextfrag.ContextFrag
	var exchangeEdits []contextfrag.ContextEditTrace
	if isRetentionIntent(profile.Intent) {
		frags, exchangeDropped, exchangeEdits = applyToolExchangePolicy(frags, budget.ToolExchange)
	}
	var result SelectionResult
	if profile.Intent != contextfrag.IntentCompactionCandidates {
		tagged := tagFragments(frags, profile)
		if isRetentionIntent(profile.Intent) {
			historyBudget := budget.MaxTokens
			trimDrops := budgetTrimDrops
			hardBudget := budget.Plan != nil || budget.EnforceProtectedBudget
			if hardBudget {
				protectedCost := protectedHistoryTokenCost(tagged)
				if protectedCost > historyBudget {
					result = selectionResultFromTagged(tagged, allSelectedIndexes(tagged))
					result.FatalError = contextfrag.ErrProtectedContextOverflow
					result = finishSelection(result, exchangeDropped, exchangeEdits, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings, gated, profile, superseded)
					return appendSystemBudgetDrops(result, systemBudgetDropped)
				}
				historyBudget -= protectedCost
				trimDrops = budgetTrimDropsEnabled
			}
			if drops, dropReasons := trimDrops(tagged, historyBudget, budget.RecentProtectTokens); len(drops) > 0 {
				if hardBudget && hasSpatialBudgetDrop(dropReasons) {
					noticeCost := contextfrag.ResolveFragTokens(TrimNoticeFrag(contextfrag.Scope{}))
					if noticeCost > historyBudget {
						result = selectionResultFromTaggedReasons(tagged, keptIndexes(tagged, drops), dropReasons)
						result.FatalError = contextfrag.ErrProtectedContextOverflow
						result = finishSelection(result, exchangeDropped, exchangeEdits, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings, gated, profile, superseded)
						return appendSystemBudgetDrops(result, systemBudgetDropped)
					}
					drops, dropReasons = budgetTrimDropsEnabled(
						tagged,
						historyBudget-noticeCost,
						budget.RecentProtectTokens,
					)
				}
				result = selectionResultFromTaggedReasons(tagged, keptIndexes(tagged, drops), dropReasons)
				if hasSpatialBudgetDrop(dropReasons) {
					result.TrimNotice = true
					result.TrimNoticeIndex = trimNoticeIndex(tagged, drops)
				}
				result = finishSelection(result, exchangeDropped, exchangeEdits, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings, gated, profile, superseded)
				return appendSystemBudgetDrops(result, systemBudgetDropped)
			}
		}
		result = selectionResultFromTagged(tagged, allSelectedIndexes(tagged))
		result = finishSelection(result, exchangeDropped, exchangeEdits, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings, gated, profile, superseded)
		return appendSystemBudgetDrops(result, systemBudgetDropped)
	}
	result = selectCompactionCandidatesWindowed(frags, profile, budget.Compaction)
	result = finishSelection(result, exchangeDropped, exchangeEdits, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings, gated, profile, superseded)
	return appendSystemBudgetDrops(result, systemBudgetDropped)
}

func finishSelection(result SelectionResult, exchangeDropped []contextfrag.ContextFrag, exchangeEdits []contextfrag.ContextEditTrace, fragBudgetDropped []fragBudgetDrop, fragBudgetEdits []fragBudgetEdit, fragBudgetWarnings []contextfrag.ValidationWarning, gated []contextfrag.ContextFrag, profile IntentProfile, superseded []conflictLoser) SelectionResult {
	result = appendToolExchangeDrops(result, exchangeDropped, exchangeEdits)
	result = appendFragBudgetDrops(result, fragBudgetDropped, fragBudgetEdits, fragBudgetWarnings)
	result = appendTrustGateDrops(result, gated, profile)
	result = appendPrecedenceDrops(result, superseded)
	return result
}

// applyTrustGate removes fragments whose trust level falls below the floor
// declared for their slot in the intent profile: instruction-bearing slots
// require workspace trust or above on provider-bound prompts, so user or
// external content can never gain instruction authority.
func applyTrustGate(frags []contextfrag.ContextFrag, profile IntentProfile) ([]contextfrag.ContextFrag, []contextfrag.ContextFrag) {
	if len(profile.SlotTrustFloors) == 0 {
		return frags, nil
	}
	kept := make([]contextfrag.ContextFrag, 0, len(frags))
	var gated []contextfrag.ContextFrag
	for _, frag := range frags {
		floor, hasFloor := profile.SlotTrustFloors[frag.Slot]
		if hasFloor && contextfrag.TrustRank(frag.Trust) < contextfrag.TrustRank(floor) {
			gated = append(gated, frag)
			continue
		}
		kept = append(kept, frag)
	}
	return kept, gated
}

func appendTrustGateDrops(result SelectionResult, gated []contextfrag.ContextFrag, profile IntentProfile) SelectionResult {
	if len(gated) == 0 {
		return result
	}
	for _, frag := range gated {
		reason := "trust_gate:" + string(frag.Slot) + "_requires_" + string(profile.SlotTrustFloors[frag.Slot])
		result.Dropped = append(result.Dropped, frag)
		result.Summary.DropReasons = append(result.Summary.DropReasons, DropRecord{
			FragID: frag.ID,
			Ref:    frag.Ref,
			Reason: reason,
		})
	}
	result.Summary.TotalCollected += len(gated)
	result.Summary.TotalDropped += len(gated)
	return result
}

type conflictLoser struct {
	frag     contextfrag.ContextFrag
	winnerID string
}

// resolveConflictGroups keeps one fragment per conflict key: the narrowest
// scope wins (closest-wins), trust breaks scope ties, and on a full tie the
// later-collected fragment supersedes the earlier one (fresher source).
func resolveConflictGroups(frags []contextfrag.ContextFrag) ([]contextfrag.ContextFrag, []conflictLoser) {
	winners := make(map[string]int, 2)
	for i, frag := range frags {
		key := frag.ConflictKey
		if key == "" {
			continue
		}
		current, exists := winners[key]
		if !exists || conflictBeats(frag, frags[current]) {
			winners[key] = i
		}
	}
	if len(winners) == 0 {
		return frags, nil
	}
	kept := make([]contextfrag.ContextFrag, 0, len(frags))
	var losers []conflictLoser
	for i, frag := range frags {
		if key := frag.ConflictKey; key != "" && winners[key] != i {
			losers = append(losers, conflictLoser{frag: frag, winnerID: frags[winners[key]].ID})
			continue
		}
		kept = append(kept, frag)
	}
	return kept, losers
}

// conflictBeats reports whether challenger supersedes incumbent; equal rank
// favors the challenger because it was collected later.
func conflictBeats(challenger, incumbent contextfrag.ContextFrag) bool {
	if cs, is := challenger.Scope.SpecificityRank(), incumbent.Scope.SpecificityRank(); cs != is {
		return cs > is
	}
	if ct, it := contextfrag.TrustRank(challenger.Trust), contextfrag.TrustRank(incumbent.Trust); ct != it {
		return ct > it
	}
	return true
}

func appendPrecedenceDrops(result SelectionResult, losers []conflictLoser) SelectionResult {
	if len(losers) == 0 {
		return result
	}
	for _, loser := range losers {
		result.Dropped = append(result.Dropped, loser.frag)
		result.Summary.DropReasons = append(result.Summary.DropReasons, DropRecord{
			FragID: loser.frag.ID,
			Ref:    loser.frag.Ref,
			Reason: "precedence:superseded_by_" + loser.winnerID,
		})
	}
	result.Summary.TotalCollected += len(losers)
	result.Summary.TotalDropped += len(losers)
	return result
}

func selectCompactionCandidatesWindowed(frags []contextfrag.ContextFrag, profile IntentProfile, window *CompactionWindow) SelectionResult {
	tagged := tagFragments(frags, profile)
	cutoff := compactionWindowCutoff(tagged, window)
	selectedIndexes := compactionSelectedIndexes(tagged, cutoff)
	if window != nil && window.MaxPromptTokens > 0 {
		selectedIndexes = trimSelectedByPromptBudget(tagged, selectedIndexes, window.MaxPromptTokens)
	}
	return selectionResultFromTagged(tagged, selectedIndexes)
}

// compactionWindowCutoff mirrors the legacy split-by-ratio/target math:
// accumulate estimated tokens newest to oldest across the whole candidate
// range and cut where the kept span reaches the window.
func compactionWindowCutoff(tagged []TaggedFrag, window *CompactionWindow) int {
	if window == nil {
		return len(tagged)
	}
	if window.TargetTokens > 0 {
		acc := 0
		for i := len(tagged) - 1; i >= 0; i-- {
			acc += fragTokenEstimate(tagged[i].Frag)
			if acc > window.TargetTokens {
				return i + 1
			}
		}
		return 0
	}
	if window.SweepAll {
		return len(tagged)
	}
	if window.KeepRecentTokens <= 0 {
		return len(tagged)
	}
	acc := 0
	for i := len(tagged) - 1; i >= 0; i-- {
		acc += fragTokenEstimate(tagged[i].Frag)
		if acc >= window.KeepRecentTokens {
			return i + 1
		}
	}
	return 0
}

// trimSelectedByPromptBudget drops the oldest selected candidates until the
// estimated prompt cost (content plus rendered metadata header) fits the
// compaction model budget, extending the cut past tool results so no orphan
// leads the kept span.
func trimSelectedByPromptBudget(tagged []TaggedFrag, selectedIndexes map[int]bool, maxTokens int) map[int]bool {
	selected := make([]int, 0, len(selectedIndexes))
	for i := range tagged {
		if selectedIndexes[i] {
			selected = append(selected, i)
		}
	}
	if len(selected) == 0 {
		return selectedIndexes
	}
	total := 0
	for _, idx := range selected {
		total += compactionPromptTokenEstimate(tagged[idx].Frag)
	}
	if total <= maxTokens {
		return selectedIndexes
	}
	acc := 0
	boundary := len(selected)
	for pos := len(selected) - 1; pos >= 0; pos-- {
		acc += compactionPromptTokenEstimate(tagged[selected[pos]].Frag)
		if acc > maxTokens {
			boundary = pos + 1
			break
		}
	}
	for boundary < len(selected) && isToolResultFrag(tagged[selected[boundary]].Frag) {
		boundary++
	}
	if boundary >= len(selected) {
		return selectedIndexes
	}
	kept := make(map[int]bool, len(selected)-boundary)
	for _, idx := range selected[boundary:] {
		kept[idx] = true
	}
	return kept
}

func compactionPromptTokenEstimate(frag contextfrag.ContextFrag) int {
	tokens := fragTokenEstimate(frag)
	if header := renderCompactionFragHeader(frag); header != "" {
		tokens += (len(header) + 3) / 4
	}
	return tokens
}

func (s *FragmentSelector) SelectCompactionCandidates(frags []contextfrag.ContextFrag, cutoff int) SelectionResult {
	profile := s.ProfileFor(contextfrag.IntentCompactionCandidates)
	return selectCompactionCandidates(frags, profile, cutoff)
}

func selectCompactionCandidates(frags []contextfrag.ContextFrag, profile IntentProfile, cutoff int) SelectionResult {
	tagged := tagFragments(frags, profile)
	selectedIndexes := compactionSelectedIndexes(tagged, cutoff)
	return selectionResultFromTagged(tagged, selectedIndexes)
}

func isRetentionIntent(intent contextfrag.Intent) bool {
	return intent == contextfrag.IntentRunConfigPreProvider ||
		intent == contextfrag.IntentDiscussReply ||
		intent == contextfrag.IntentACPRuntimePrompt
}

func selectionResultFromTagged(tagged []TaggedFrag, selectedIndexes map[int]bool) SelectionResult {
	return selectionResultFromTaggedReasons(tagged, selectedIndexes, nil)
}

func selectionResultFromTaggedReasons(tagged []TaggedFrag, selectedIndexes map[int]bool, reasonOverrides map[int]string) SelectionResult {
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

// trimNoticeIndex returns the position within the selected slice where the
// trim notice belongs: the closure-safe boundary nearest to the first kept
// fragment that follows the last dropped one, so the notice never separates a
// kept tool call from its results.
func trimNoticeIndex(tagged []TaggedFrag, drops map[int]bool) int {
	lastDropped := -1
	for i := range tagged {
		if drops[i] {
			lastDropped = i
		}
	}
	if lastDropped < 0 {
		return -1
	}
	kept := make([]contextfrag.ContextFrag, 0, len(tagged)-len(drops))
	base := -1
	for i := range tagged {
		if drops[i] {
			continue
		}
		if base < 0 && i > lastDropped {
			base = len(kept)
		}
		kept = append(kept, tagged[i].Frag)
	}
	if base < 0 {
		base = len(kept)
	}
	return closureSafeNoticeIndex(kept, base)
}

// closureSafeNoticeIndex picks the position closest to base where no tool
// call opened earlier still awaits its result: the smallest safe position at
// or after base, else the largest safe one before it (position 0 is always
// safe because nothing precedes it).
func closureSafeNoticeIndex(kept []contextfrag.ContextFrag, base int) int {
	open := make(map[string]int)
	fallback := 0
	for pos := 0; pos <= len(kept); pos++ {
		if len(open) == 0 {
			if pos >= base {
				return pos
			}
			fallback = pos
		}
		if pos == len(kept) {
			break
		}
		for _, id := range fragToolCallIDs(kept[pos]) {
			open[id]++
		}
		for _, id := range fragToolResultCallIDs(kept[pos]) {
			if count, ok := open[id]; ok {
				if count <= 1 {
					delete(open, id)
				} else {
					open[id] = count - 1
				}
			}
		}
	}
	return fallback
}

func allSelectedIndexes(tagged []TaggedFrag) map[int]bool {
	selected := make(map[int]bool, len(tagged))
	for i := range tagged {
		selected[i] = true
	}
	return selected
}

func compactionSelectedIndexes(tagged []TaggedFrag, cutoff int) map[int]bool {
	selected := make(map[int]bool)
	if len(tagged) == 0 {
		return selected
	}
	if cutoff > len(tagged) {
		cutoff = len(tagged)
	}
	if latestUser := latestUserIndex(tagged); latestUser == 0 && len(tagged) > 1 {
		protectedTailStart := firstTagStartAfter(tagged, TagPreserveRecent, 1)
		if protectedTailStart <= 1 {
			return selected
		}
		if cutoff > protectedTailStart {
			cutoff = protectedTailStart
		}
		if cutoff <= 1 {
			return selected
		}
		cutoff = adjustToolBoundary(tagged, cutoff)
		if cutoff > protectedTailStart {
			return selected
		}
		for i := 1; i < cutoff; i++ {
			if tagged[i].HasTag(TagCanDrop) {
				selected[i] = true
			}
		}
		return selected
	}

	protectedStart := firstCompactionProtectedStart(tagged)
	if protectedStart <= 0 {
		return selected
	}
	if cutoff > protectedStart {
		cutoff = protectedStart
	}
	cutoff = adjustToolBoundary(tagged, cutoff)
	if cutoff > protectedStart || cutoff <= 0 {
		return selected
	}
	for i := 0; i < cutoff; i++ {
		if tagged[i].HasTag(TagCanDrop) {
			selected[i] = true
		}
	}
	return selected
}

func firstCompactionProtectedStart(tagged []TaggedFrag) int {
	for i, taggedFrag := range tagged {
		if !taggedFrag.HasTag(TagPreserveRecent) {
			continue
		}
		if isOutOfBandMustKeepSlot(taggedFrag.Frag) {
			continue
		}
		return i
	}
	return len(tagged)
}

func isOutOfBandMustKeepSlot(frag contextfrag.ContextFrag) bool {
	return frag.Slot == contextfrag.SlotSystem || frag.Slot == contextfrag.SlotCurrentUser
}

func firstTagStartAfter(tagged []TaggedFrag, tag SelectionTag, start int) int {
	if start < 0 {
		start = 0
	}
	for i, taggedFrag := range tagged {
		if i < start {
			continue
		}
		if taggedFrag.HasTag(tag) {
			return i
		}
	}
	return len(tagged)
}

func adjustToolBoundary(tagged []TaggedFrag, cutoff int) int {
	for cutoff > 0 && cutoff < len(tagged) && isToolClosureResult(tagged[cutoff]) {
		cutoff++
	}
	return cutoff
}
