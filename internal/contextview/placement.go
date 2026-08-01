package contextview

import (
	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

type StablePrefixPlacer struct{}

func (StablePrefixPlacer) Place(selected []contextfrag.ContextFrag, _ contextfrag.Intent) PlacementPlan {
	items := make([]PlacementItem, 0, len(selected))
	firstVolatile := len(selected)
	for i, frag := range selected {
		ref := placementRef(frag)
		items = append(items, PlacementItem{
			FragID:    frag.ID,
			Slot:      frag.Slot,
			Position:  i,
			CacheHint: frag.CacheClass,
			Ref:       ref,
		})
		if firstVolatile == len(selected) && frag.CacheClass != contextfrag.CacheStable {
			firstVolatile = i
		}
	}
	return PlacementPlan{
		StablePrefixHash:   stablePrefixHash(selected[:firstVolatile]),
		FirstVolatileIndex: firstVolatile,
		Items:              items,
	}
}

func placementRef(frag contextfrag.ContextFrag) contextfrag.ContextRef {
	if err := contextfrag.ValidateContextRef(frag.Ref); err == nil {
		return frag.Ref
	}
	return contextfrag.WithContextRef(frag, frag.Ref).Ref
}

func stablePrefixHash(frags []contextfrag.ContextFrag) string {
	if len(frags) == 0 {
		return ""
	}
	hash, err := sdkRenderedPayloadHash(renderSDKPayloadFromFrags(frags))
	if err != nil {
		return ""
	}
	return hash
}
