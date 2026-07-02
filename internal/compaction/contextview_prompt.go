package compaction

import (
	"context"
	"fmt"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/historyfrag"
)

func contextViewCompactionPrompt(toCompact []RecordCompactionCandidate, priorSummaries []string) (*contextview.CompactionRenderedPayload, error) {
	frags := make([]contextfrag.ContextFrag, 0, len(toCompact))
	for _, candidate := range toCompact {
		frags = append(frags, historyfrag.ToFrag(candidate.Record))
	}
	placement := contextview.IdentityPlacer{}.Place(frags, contextfrag.IntentCompactionCandidates)
	renderer := &contextview.CompactionPromptRenderer{PriorSummaries: priorSummaries}
	rendered, err := renderer.Render(context.Background(), contextview.RenderInput{
		Intent:    contextfrag.IntentCompactionCandidates,
		Selected:  frags,
		Placement: placement,
		Target:    contextfrag.RenderCompactionPrompt,
	})
	if err != nil {
		return nil, err
	}
	payload, ok := rendered.Data.(*contextview.CompactionRenderedPayload)
	if !ok {
		return nil, fmt.Errorf("unexpected compaction payload type %T", rendered.Data)
	}
	return payload, nil
}

// contextViewSelectionDivergence reports toCompact refs that the contextview
// selection engine would not consider droppable. A non-empty result means the
// legacy token windowing and the fragment selector disagree on eligibility.
func contextViewSelectionDivergence(all []RecordCompactionCandidate, toCompact []RecordCompactionCandidate) []string {
	if len(toCompact) == 0 {
		return nil
	}
	frags := make([]contextfrag.ContextFrag, 0, len(all))
	for _, candidate := range all {
		frags = append(frags, historyfrag.ToFrag(candidate.Record))
	}
	selector := &contextview.FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentCompactionCandidates)
	result := selector.Select(frags, profile, contextview.BudgetEnvelope{})

	eligible := make(map[string]bool, len(result.Selected))
	for _, frag := range result.Selected {
		eligible[frag.Ref.StableKey()] = true
	}
	var divergent []string
	for _, candidate := range toCompact {
		frag := historyfrag.ToFrag(candidate.Record)
		if !eligible[frag.Ref.StableKey()] {
			divergent = append(divergent, candidate.Record.Ref.ID)
		}
	}
	return divergent
}
