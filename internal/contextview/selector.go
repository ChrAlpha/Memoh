package contextview

import "github.com/memohai/memoh/internal/contextfrag"

type FragmentSelector struct{}

func (s *FragmentSelector) ProfileFor(intent contextfrag.Intent) IntentProfile {
	switch intent {
	case contextfrag.IntentCompactionCandidates:
		return IntentProfile{
			Intent:        intent,
			MustKeepSlots: []contextfrag.Slot{contextfrag.SlotSystem, contextfrag.SlotCurrentUser},
		}
	default:
		return IntentProfile{Intent: intent}
	}
}

func (s *FragmentSelector) Select(frags []contextfrag.ContextFrag, profile IntentProfile, _ BudgetEnvelope) SelectionResult {
	if profile.Intent != contextfrag.IntentCompactionCandidates {
		selected := append([]contextfrag.ContextFrag(nil), frags...)
		return SelectionResult{
			Selected: selected,
			Summary: SelectionSummary{
				TotalCollected: len(frags),
				TotalSelected:  len(selected),
			},
		}
	}

	tagged := tagFragments(frags, profile)
	selectedIndexes := compactionSelectedIndexes(tagged)
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
			TotalCollected: len(frags),
			TotalSelected:  len(selected),
			TotalDropped:   len(dropped),
			DropReasons:    dropRecords,
		},
	}
}

func compactionSelectedIndexes(tagged []TaggedFrag) map[int]bool {
	selected := make(map[int]bool)
	if len(tagged) == 0 {
		return selected
	}
	if latestUser := latestUserIndex(tagged); latestUser == 0 && len(tagged) > 1 {
		protectedTailStart := firstTagStartAfter(tagged, TagPreserveRecent, 1)
		if protectedTailStart <= 1 {
			return selected
		}
		cutoff := adjustToolBoundary(tagged, protectedTailStart)
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
	cutoff := adjustToolBoundary(tagged, protectedStart)
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
