package application

import (
	"fmt"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func contextSwitchCompactionScope(input native.ContextSwitchInput) ([]contextfrag.ContextRef, int, error) {
	type span struct{ start, end, cost int }
	var spans []span
	var protected []sdk.Message
	for i := 0; i < len(input.Sources); {
		if input.Protected[i] || len(contextSwitchSourceCoverage(input.Sources[i])) == 0 {
			protected = append(protected, input.Request.Messages[i])
			i++
			continue
		}
		start := i
		for i < len(input.Sources) && !input.Protected[i] && len(contextSwitchSourceCoverage(input.Sources[i])) > 0 {
			i++
		}
		spans = append(spans, span{start: start, end: i, cost: contextfrag.ProviderEnvelopeTokens("", input.Request.Messages[start:i], nil)})
	}
	if len(spans) == 0 {
		return nil, 0, nil
	}
	protectedCost := contextfrag.ProviderEnvelopeTokens(input.Request.System, protected, input.Request.Tools)
	if protectedCost >= input.InputAllowanceTokens {
		return nil, 0, fmt.Errorf("%w: protected context leaves no summary allowance", contextfrag.ErrProtectedContextOverflow)
	}
	selected := spans[0]
	for _, candidate := range spans[1:] {
		if candidate.cost > selected.cost {
			selected = candidate
		}
	}
	var refs []contextfrag.ContextRef
	for _, source := range input.Sources[selected.start:selected.end] {
		refs = append(refs, contextSwitchSourceCoverage(source)...)
	}
	fixed := append([]sdk.Message(nil), input.Request.Messages[:selected.start]...)
	fixed = append(fixed, input.Request.Messages[selected.end:]...)
	budget := input.InputAllowanceTokens - contextfrag.ProviderEnvelopeTokens(input.Request.System, fixed, input.Request.Tools)
	if budget <= 0 {
		budget = max(1, (input.InputAllowanceTokens-protectedCost)/len(spans))
	}
	return refs, budget, nil
}
