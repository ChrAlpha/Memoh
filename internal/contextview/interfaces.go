package contextview

import (
	"context"

	"github.com/memohai/memoh/internal/contextfrag"
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
	Summary         SelectionSummary
}

type IntentProfile struct {
	Intent        contextfrag.Intent
	RequiredKinds []contextfrag.Kind
	MustKeepSlots []contextfrag.Slot
	// RejectExternalSystemFrags drops external-trust fragments from the
	// system slot before selection: provider-bound system prompts carry
	// instruction authority, so untrusted content must never enter them.
	RejectExternalSystemFrags bool
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
