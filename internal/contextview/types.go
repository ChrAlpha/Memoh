package contextview

import "github.com/memohai/memoh/internal/contextfrag"

type BuildInput struct {
	Scope   contextfrag.Scope
	Intent  contextfrag.Intent
	Sources []SourceSpec
	Targets []contextfrag.RenderTarget
	Budget  BudgetEnvelope
	Options BuildOptions
}

type SourceSpec struct {
	Name   string
	Config map[string]any
}

type BudgetEnvelope struct {
	MaxTokens     int
	MaxChars      int
	MaxImages     int
	MaxToolSchema int
}

type BuildOptions struct {
	DryRun       bool
	ShadowLegacy bool
}

type ContextView struct {
	Intent      contextfrag.Intent
	SourceFrags []contextfrag.ContextFrag
	Selected    []contextfrag.ContextFrag
	Placement   PlacementPlan
	Manifest    contextfrag.Manifest
	Rendered    map[contextfrag.RenderTarget]RenderedPayload
	Trace       BuildTrace
}

type RenderedPayload struct {
	Target      contextfrag.RenderTarget
	ContentHash string
	Data        any
}

type PlacementPlan struct {
	StablePrefixHash   string
	FirstVolatileIndex int
	Items              []PlacementItem
}

type PlacementItem struct {
	FragID    string
	Slot      contextfrag.Slot
	Position  int
	CacheHint contextfrag.CacheClass
	Ref       contextfrag.ContextRef
}

type BuildTrace struct {
	CollectDurations map[string]int64
	SelectionSummary SelectionSummary
	PlacementSummary PlacementSummary
	RenderSummaries  []RenderSummary
	Warnings         []contextfrag.ValidationWarning
}

type SelectionSummary struct {
	TotalCollected int
	TotalSelected  int
	TotalDropped   int
	DropReasons    []DropRecord
}

type DropRecord struct {
	FragID string
	Ref    contextfrag.ContextRef
	Reason string
}

type PlacementSummary struct {
	StablePrefixFrags int
	DynamicFrags      int
}

type RenderSummary struct {
	Target      contextfrag.RenderTarget
	ContentHash string
	ItemCount   int
}
