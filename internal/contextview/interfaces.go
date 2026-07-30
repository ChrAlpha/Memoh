package contextview

import (
	"context"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

type CollectRequest struct {
	Scope  contextfrag.Scope
	Intent contextfrag.Intent
	Config any
}

type RenderInput struct {
	Intent    contextfrag.Intent
	Selected  []contextfrag.ContextFrag
	Placement PlacementPlan
	Manifest  *contextfrag.Manifest
	Scope     contextfrag.Scope
	Target    contextfrag.RenderTarget
}

type Collector interface {
	Name() string
	Collect(context.Context, CollectRequest) ([]contextfrag.ContextFrag, error)
}

type CollectorRegistry interface {
	Get(name string) (Collector, bool)
	Names() []string
}

type Selector interface {
	ProfileFor(contextfrag.Intent) IntentProfile
	Select([]contextfrag.ContextFrag, IntentProfile, BudgetEnvelope) SelectionResult
}

type SelectionResult struct {
	Selected []contextfrag.ContextFrag
	Dropped  []contextfrag.ContextFrag
	// TrimNotice reports that budget trimming dropped history and the builder
	// must splice the trim notice into Selected at TrimNoticeIndex.
	TrimNotice      bool
	TrimNoticeIndex int
	// Edited records in-place content edits (e.g. tool-call stripping) applied
	// to kept fragments, for the manifest edit trace.
	Edited []contextfrag.ContextEditTrace
	// Warnings records validation warnings raised during selection (e.g. an
	// overflow policy the selector cannot enforce), for the manifest.
	Warnings []contextfrag.ValidationWarning
	Summary  SelectionSummary
}

type IntentProfile struct {
	Intent        contextfrag.Intent
	MustKeepSlots []contextfrag.Slot
	// MustKeepFrag evaluates retention that depends on fragment policy rather
	// than provider placement alone.
	MustKeepFrag func(contextfrag.ContextFrag) bool
	// SlotTrustFloors declares the minimum trust level per slot: fragments
	// below the floor are dropped before selection so content never gains
	// instruction authority its provenance does not warrant.
	SlotTrustFloors map[contextfrag.Slot]contextfrag.TrustLevel
}

type Placer interface {
	Place([]contextfrag.ContextFrag, contextfrag.Intent) PlacementPlan
}

type Renderer interface {
	Target() contextfrag.RenderTarget
	Render(context.Context, RenderInput) (RenderedPayload, error)
}

type RendererRegistry interface {
	Get(contextfrag.RenderTarget) (Renderer, bool)
}
