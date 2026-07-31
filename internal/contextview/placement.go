package contextview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

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
	items := make([]stablePrefixHashItem, 0, len(frags))
	for _, frag := range frags {
		items = append(items, stablePrefixHashItem{
			ID:         frag.ID,
			Ref:        placementRef(frag),
			Slot:       frag.Slot,
			CacheClass: frag.CacheClass,
		})
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type stablePrefixHashItem struct {
	ID         string                 `json:"id"`
	Ref        contextfrag.ContextRef `json:"ref"`
	Slot       contextfrag.Slot       `json:"slot"`
	CacheClass contextfrag.CacheClass `json:"cache_class"`
}
