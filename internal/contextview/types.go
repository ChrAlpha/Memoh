package contextview

import contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"

type BuildInput struct {
	Scope           contextfrag.Scope
	Intent          contextfrag.Intent
	Sources         []SourceSpec
	Targets         []contextfrag.RenderTarget
	Budget          BudgetEnvelope
	DynamicMutators []contextfrag.DynamicMutator
	Options         BuildOptions
	// Mutations is the run's live mutation ledger; renderers record
	// post-selection changes such as a final render prune onto it.
	Mutations *contextfrag.MutationLedger
}

type SourceSpec struct {
	Name   string
	Config any
}

// BudgetEnvelope prices a view against the provider allowance. Selection
// keeps every history fragment whatever the budget: a request that does not
// fit is compacted or fails, never trimmed. Plan, when set, bounds the system
// prompt to its share of the window, and the rendered request is checked
// against it; nil leaves the view unbudgeted.
type BudgetEnvelope struct {
	Plan *contextfrag.ContextBudgetPlan
}

type BuildOptions struct {
	DryRun bool
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
	Target contextfrag.RenderTarget
	Data   any
}

type PlacementPlan struct {
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
	RenderSummaries  map[contextfrag.RenderTarget]RenderSummary
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
	Target    contextfrag.RenderTarget
	ItemCount int
}
