package contextview

import "github.com/memohai/memoh/internal/contextfrag"

type BuildInput struct {
	Intent        contextfrag.Intent
	Sources       []SourceSpec
	Scope         contextfrag.Scope
	Budget        BudgetEnvelope
	Options       BuildOptions
	RenderTargets []contextfrag.RenderTarget
	Metadata      map[string]string
}

type SourceSpec struct {
	Name     string
	Ref      contextfrag.ContextRef
	Budget   BudgetEnvelope
	Metadata map[string]string
}

type BudgetEnvelope struct {
	MaxTokens int
	MaxChars  int
}

type BuildOptions struct {
	DryRun bool
}

type ContextView struct {
	Intent    contextfrag.Intent
	Profile   IntentProfile
	Frags     []contextfrag.ContextFrag
	Manifest  contextfrag.Manifest
	Placement PlacementPlan
	Rendered  []RenderedPayload
	Trace     BuildTrace
	Warnings  []string
}

type RenderedPayload struct {
	Target      contextfrag.RenderTarget
	ContentHash string
	ItemCount   int
	Payload     any
}

type PlacementPlan struct {
	Items   []PlacementItem
	Summary PlacementSummary
}

type PlacementItem struct {
	Index int
	Frag  contextfrag.ContextFrag
	Slot  contextfrag.Slot
}

type BuildTrace struct {
	CollectDurations map[string]int64
	Selection        SelectionSummary
	Placement        PlacementSummary
	Render           []RenderSummary
}

type SelectionSummary struct {
	InputCount    int
	SelectedCount int
	DroppedCount  int
}

type DropRecord struct {
	FragID string
	Reason string
}

type PlacementSummary struct {
	ItemCount int
}

type RenderSummary struct {
	Target      contextfrag.RenderTarget
	ContentHash string
	ItemCount   int
}
