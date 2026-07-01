package contextview

import (
	"context"

	"github.com/memohai/memoh/internal/contextfrag"
)

type CollectRequest struct {
	Scope  contextfrag.Scope
	Intent contextfrag.Intent
	Config map[string]any
}

type RenderInput struct {
	Target    contextfrag.RenderTarget
	Intent    contextfrag.Intent
	Selected  []contextfrag.ContextFrag
	Placement PlacementPlan
	Manifest  contextfrag.Manifest
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
	Summary  SelectionSummary
	Warnings []contextfrag.ValidationWarning
}

type IntentProfile struct {
	Intent contextfrag.Intent
	View   contextfrag.ManifestView
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
