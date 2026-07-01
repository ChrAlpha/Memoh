package contextview

import (
	"context"

	"github.com/memohai/memoh/internal/contextfrag"
)

type Collector interface {
	Collect(context.Context, BuildInput, SourceSpec) ([]contextfrag.ContextFrag, error)
}

type CollectorRegistry interface {
	Collector(name string) (Collector, bool)
	Names() []string
}

type Selector interface {
	ProfileFor(contextfrag.Intent) IntentProfile
	Select(context.Context, IntentProfile, BuildInput, []contextfrag.ContextFrag) (SelectionResult, error)
}

type SelectionResult struct {
	Frags    []contextfrag.ContextFrag
	Summary  SelectionSummary
	Drops    []DropRecord
	Warnings []string
}

type IntentProfile struct {
	Intent        contextfrag.Intent
	View          contextfrag.ManifestView
	Budget        BudgetEnvelope
	RenderTargets []contextfrag.RenderTarget
}

type Placer interface {
	Place(context.Context, IntentProfile, BuildInput, []contextfrag.ContextFrag) (PlacementPlan, error)
}

type Renderer interface {
	Render(context.Context, contextfrag.RenderTarget, BuildInput, ContextView) (RenderedPayload, error)
}

type RendererRegistry interface {
	Renderer(contextfrag.RenderTarget) (Renderer, bool)
	Names() []contextfrag.RenderTarget
}
