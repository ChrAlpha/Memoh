package compaction

import (
	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/turn"
)

// splice applies a committed pass to a request built from rows, as a run
// applies it to its own: the messages and sources pass.Order keeps, in its
// order, with the summary in its place.
func splice(messages []sdk.Message, sources []Source, pass Pass) ([]sdk.Message, []Source) {
	outMessages := make([]sdk.Message, 0, len(pass.Order))
	outSources := make([]Source, 0, len(pass.Order))
	for _, i := range pass.Order {
		if i < 0 {
			outMessages = append(outMessages, SummaryMessage(pass.Summary))
			outSources = append(outSources, Source{Sources: []turn.ContextMessageSource{{Kind: "summary", ID: pass.SummaryID}}})
			continue
		}
		outMessages = append(outMessages, messages[i])
		outSources = append(outSources, sources[i])
	}
	return outMessages, outSources
}
