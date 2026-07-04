package contextview

import "github.com/memohai/memoh/internal/contextfrag"

type BuildInput struct {
	Scope           contextfrag.Scope
	Intent          contextfrag.Intent
	Sources         []SourceSpec
	Targets         []contextfrag.RenderTarget
	Budget          BudgetEnvelope
	DynamicMutators []contextfrag.DynamicMutator
	Options         BuildOptions
}

type SourceSpec struct {
	Name   string
	Config any
}

type BudgetEnvelope struct {
	MaxTokens     int
	MaxChars      int
	MaxImages     int
	MaxToolSchema int
	// ToolExchange strips bulky tool interactions from history (ask_user
	// survives); nil keeps every exchange.
	ToolExchange *contextfrag.ToolExchangePolicy
	// Compaction carries the compaction-candidate windowing decision inputs;
	// nil means the whole eligible range is in scope.
	Compaction *CompactionWindow
}

// CompactionWindow mirrors the legacy ratio/target/trim windowing that
// decides how much history enters a compaction pass.
type CompactionWindow struct {
	// SweepAll marks a full sweep (legacy ratio >= 100).
	SweepAll bool
	// KeepRecentTokens keeps this many newest tokens out of compaction.
	KeepRecentTokens int
	// TargetTokens compacts everything older than the newest span that fits
	// within this many tokens (sync compaction).
	TargetTokens int
	// MaxPromptTokens caps the selected candidates' total prompt cost by
	// dropping the oldest selected entries.
	MaxPromptTokens int
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
	Target      contextfrag.RenderTarget
	ContentHash string
	ItemCount   int
}
