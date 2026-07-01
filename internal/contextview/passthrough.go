package contextview

import (
	"context"
	"sort"

	"github.com/memohai/memoh/internal/contextfrag"
)

type PassthroughSelector struct{}

func (PassthroughSelector) ProfileFor(intent contextfrag.Intent) IntentProfile {
	return IntentProfile{
		Intent: intent,
		View:   manifestViewForIntent(intent),
	}
}

func (PassthroughSelector) Select(_ context.Context, _ IntentProfile, _ BuildInput, frags []contextfrag.ContextFrag) (SelectionResult, error) {
	selected := append([]contextfrag.ContextFrag(nil), frags...)
	return SelectionResult{
		Frags: selected,
		Summary: SelectionSummary{
			InputCount:    len(frags),
			SelectedCount: len(selected),
		},
	}, nil
}

type IdentityPlacer struct{}

func (IdentityPlacer) Place(_ context.Context, _ IntentProfile, _ BuildInput, frags []contextfrag.ContextFrag) (PlacementPlan, error) {
	items := make([]PlacementItem, 0, len(frags))
	for i, frag := range frags {
		items = append(items, PlacementItem{
			Index: i,
			Frag:  frag,
			Slot:  frag.Slot,
		})
	}
	return PlacementPlan{
		Items:   items,
		Summary: PlacementSummary{ItemCount: len(items)},
	}, nil
}

type NoopRenderer struct{}

func (NoopRenderer) Render(_ context.Context, target contextfrag.RenderTarget, _ BuildInput, _ ContextView) (RenderedPayload, error) {
	return RenderedPayload{Target: target}, nil
}

type MapCollectorRegistry struct {
	collectors map[string]Collector
}

func NewMapCollectorRegistry(collectors map[string]Collector) MapCollectorRegistry {
	cloned := make(map[string]Collector, len(collectors))
	for name, collector := range collectors {
		cloned[name] = collector
	}
	return MapCollectorRegistry{collectors: cloned}
}

func (r MapCollectorRegistry) Collector(name string) (Collector, bool) {
	collector, ok := r.collectors[name]
	if !ok || collector == nil {
		return nil, false
	}
	return collector, true
}

func (r MapCollectorRegistry) Names() []string {
	names := make([]string, 0, len(r.collectors))
	for name := range r.collectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type MapRendererRegistry struct {
	renderers map[contextfrag.RenderTarget]Renderer
}

func NewMapRendererRegistry(renderers map[contextfrag.RenderTarget]Renderer) MapRendererRegistry {
	cloned := make(map[contextfrag.RenderTarget]Renderer, len(renderers))
	for target, renderer := range renderers {
		cloned[target] = renderer
	}
	return MapRendererRegistry{renderers: cloned}
}

func (r MapRendererRegistry) Renderer(target contextfrag.RenderTarget) (Renderer, bool) {
	renderer, ok := r.renderers[target]
	if !ok || renderer == nil {
		return nil, false
	}
	return renderer, true
}

func (r MapRendererRegistry) Names() []contextfrag.RenderTarget {
	names := make([]contextfrag.RenderTarget, 0, len(r.renderers))
	for target := range r.renderers {
		names = append(names, target)
	}
	sort.Slice(names, func(i, j int) bool {
		return names[i] < names[j]
	})
	return names
}

type StaticCollector struct {
	Frags []contextfrag.ContextFrag
	Err   error
}

func (c StaticCollector) Collect(_ context.Context, _ BuildInput, _ SourceSpec) ([]contextfrag.ContextFrag, error) {
	if c.Err != nil {
		return nil, c.Err
	}
	return append([]contextfrag.ContextFrag(nil), c.Frags...), nil
}
