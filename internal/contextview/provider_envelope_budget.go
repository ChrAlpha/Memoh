package contextview

import (
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

func providerToolDefsCost(definitions []contextfrag.ToolDefAccounting) int {
	total := 0
	for _, definition := range definitions {
		total += contextfrag.ProviderToolDefTokens(definition)
	}
	return total
}
