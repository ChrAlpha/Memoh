package contextview

import "github.com/memohai/memoh/internal/contextfrag"

type FragmentSelector struct{}

func (*FragmentSelector) ProfileFor(intent contextfrag.Intent) IntentProfile {
	switch intent {
	case contextfrag.IntentRunConfigPreProvider:
		return IntentProfile{
			Intent:                    intent,
			MustKeepSlots:             []contextfrag.Slot{contextfrag.SlotSystem, contextfrag.SlotCurrentUser},
			RejectExternalSystemFrags: true,
		}
	case contextfrag.IntentCompactionCandidates:
		return IntentProfile{
			Intent:        intent,
			MustKeepSlots: []contextfrag.Slot{contextfrag.SlotSystem, contextfrag.SlotCurrentUser},
		}
	case contextfrag.IntentDiscussReply:
		return IntentProfile{
			Intent:                    intent,
			MustKeepSlots:             []contextfrag.Slot{contextfrag.SlotSystem, contextfrag.SlotCurrentUser},
			RejectExternalSystemFrags: true,
		}
	case contextfrag.IntentACPRuntimePrompt:
		// The ACP prompt is one rendered document: its system slot marks
		// document position, not instruction authority, so external-trust
		// sections (attachment metadata) legitimately live there.
		return IntentProfile{
			Intent:        intent,
			MustKeepSlots: []contextfrag.Slot{contextfrag.SlotSystem, contextfrag.SlotCurrentUser},
		}
	default:
		return IntentProfile{Intent: intent}
	}
}

func (*FragmentSelector) Select(frags []contextfrag.ContextFrag, profile IntentProfile, budget BudgetEnvelope) SelectionResult {
	frags, gated := applyTrustGate(frags, profile)
	var exchangeDropped []contextfrag.ContextFrag
	var exchangeEdits []contextfrag.ContextEditTrace
	if isRetentionIntent(profile.Intent) {
		frags, exchangeDropped, exchangeEdits = applyToolExchangePolicy(frags, budget.ToolExchange)
	}
	var result SelectionResult
	if profile.Intent != contextfrag.IntentCompactionCandidates {
		tagged := tagFragments(frags, profile)
		if isRetentionIntent(profile.Intent) {
			if drops := budgetTrimDrops(tagged, budget.MaxTokens); len(drops) > 0 {
				result = selectionResultFromTagged(tagged, keptIndexes(tagged, drops))
				result.TrimNotice = true
				result.TrimNoticeIndex = trimNoticeIndex(tagged, drops)
				return appendTrustGateDrops(appendToolExchangeDrops(result, exchangeDropped, exchangeEdits), gated)
			}
		}
		result = selectionResultFromTagged(tagged, allSelectedIndexes(tagged))
		return appendTrustGateDrops(appendToolExchangeDrops(result, exchangeDropped, exchangeEdits), gated)
	}
	result = selectCompactionCandidatesWindowed(frags, profile, budget.Compaction)
	return appendTrustGateDrops(appendToolExchangeDrops(result, exchangeDropped, exchangeEdits), gated)
}

const trustGateExternalSystemReason = "trust_gate:external_in_system_slot"

// applyTrustGate removes fragments whose trust level is not allowed to
// occupy their slot for this intent. Today's single rule: external-trust
// content must never enter the system slot of a provider-bound prompt.
func applyTrustGate(frags []contextfrag.ContextFrag, profile IntentProfile) ([]contextfrag.ContextFrag, []contextfrag.ContextFrag) {
	if !profile.RejectExternalSystemFrags {
		return frags, nil
	}
	kept := make([]contextfrag.ContextFrag, 0, len(frags))
	var gated []contextfrag.ContextFrag
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem && frag.Trust == contextfrag.TrustExternal {
			gated = append(gated, frag)
			continue
		}
		kept = append(kept, frag)
	}
	return kept, gated
}

func appendTrustGateDrops(result SelectionResult, gated []contextfrag.ContextFrag) SelectionResult {
	if len(gated) == 0 {
		return result
	}
	for _, frag := range gated {
		result.Dropped = append(result.Dropped, frag)
		result.Summary.DropReasons = append(result.Summary.DropReasons, DropRecord{
			FragID: frag.ID,
			Ref:    frag.Ref,
			Reason: trustGateExternalSystemReason,
		})
	}
	result.Summary.TotalCollected += len(gated)
	result.Summary.TotalDropped += len(gated)
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
	selected := make([]contextfrag.ContextFrag, 0, len(selectedIndexes))
	dropped := make([]contextfrag.ContextFrag, 0, len(tagged)-len(selectedIndexes))
	dropRecords := make([]DropRecord, 0, len(tagged)-len(selectedIndexes))

	for i, taggedFrag := range tagged {
		if selectedIndexes[i] {
			selected = append(selected, taggedFrag.Frag)
			continue
		}
		dropped = append(dropped, taggedFrag.Frag)
		dropRecords = append(dropRecords, DropRecord{
			FragID: taggedFrag.Frag.ID,
			Ref:    taggedFrag.Frag.Ref,
			Reason: selectionDropReason(taggedFrag),
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
// trim notice belongs: immediately before the first kept fragment that
// follows the last dropped one.
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
	selectedPos := 0
	for i := range tagged {
		if drops[i] {
			continue
		}
		if i > lastDropped {
			return selectedPos
		}
		selectedPos++
	}
	return selectedPos
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
