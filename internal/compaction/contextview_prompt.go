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
