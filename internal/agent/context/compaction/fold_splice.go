package compaction

import (
	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/turn"
)

// Splice applies a committed pass to a request the way a run applies it to
// its own: a message goes when every source it renders is covered or
// absorbed, the tail from cut on and runtime context stay, and the summary
// takes the place turn.PlaceSummary gives it. A pass covers only the anchors
// beyond its cap. It returns the spliced messages, their sources and the cut
// moved with them.
func Splice(messages []sdk.Message, sources []Source, cut int, pass Pass) ([]sdk.Message, []Source, int) {
	covered := make(map[turn.ContextMessageSource]bool, len(pass.Covered)+len(pass.Absorbed))
	for _, source := range pass.Covered {
		covered[turn.ContextMessageSource{Kind: source.Kind, ID: source.ID}] = true
	}
	for _, id := range pass.Absorbed {
		covered[turn.ContextMessageSource{Kind: "summary", ID: id}] = true
	}
	removed := func(i int) bool {
		source := sources[i]
		if i >= cut || len(source.Sources) == 0 {
			return false
		}
		for _, s := range source.Sources {
			if !covered[turn.ContextMessageSource{Kind: s.Kind, ID: s.ID}] {
				return false
			}
		}
		return true
	}
	order := make([]int, len(messages))
	for i := range order {
		order[i] = i
	}
	order, placed := turn.PlaceSummary(order, -1, removed)
	if !placed {
		return messages, sources, cut
	}
	outMessages := make([]sdk.Message, 0, len(order))
	outSources := make([]Source, 0, len(order))
	outCut := -1
	for _, i := range order {
		if i == cut {
			outCut = len(outMessages)
		}
		if i < 0 {
			outMessages = append(outMessages, SummaryMessage(pass.Summary))
			outSources = append(outSources, Source{Sources: []turn.ContextMessageSource{{Kind: "summary", ID: pass.ArtifactID}}})
			continue
		}
		outMessages = append(outMessages, messages[i])
		outSources = append(outSources, sources[i])
	}
	if outCut < 0 {
		outCut = len(outMessages)
	}
	return outMessages, outSources, outCut
}
