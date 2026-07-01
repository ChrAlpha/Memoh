package contextview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/memohai/memoh/internal/contextfrag"
)

type Builder struct {
	collectors CollectorRegistry
	selector   Selector
	placer     Placer
	renderers  RendererRegistry
}

func NewBuilder(collectors CollectorRegistry, selector Selector, placer Placer, renderers RendererRegistry) *Builder {
	return &Builder{
		collectors: collectors,
		selector:   selector,
		placer:     placer,
		renderers:  renderers,
	}
}

func (b *Builder) Build(ctx context.Context, input BuildInput) (*ContextView, error) {
	if b.collectors == nil {
		return nil, errors.New("collector registry is required")
	}
	if b.selector == nil {
		return nil, errors.New("selector is required")
	}
	if b.placer == nil {
		return nil, errors.New("placer is required")
	}

	trace := BuildTrace{
		CollectDurations: make(map[string]int64, len(input.Sources)),
		RenderSummaries:  make(map[contextfrag.RenderTarget]RenderSummary, len(input.Targets)),
	}
	sourceFrags := make([]contextfrag.ContextFrag, 0)
	for _, spec := range input.Sources {
		name := strings.TrimSpace(spec.Name)
		collector, ok := b.collectors.Get(name)
		if !ok {
			return nil, fmt.Errorf("unknown collector %q", name)
		}

		start := time.Now()
		frags, err := collector.Collect(ctx, CollectRequest{
			Scope:  input.Scope,
			Intent: input.Intent,
			Config: spec.Config,
		})
		trace.CollectDurations[name] = time.Since(start).Microseconds()
		if err != nil {
			return nil, fmt.Errorf("collector %q: %w", name, err)
		}
		sourceFrags = append(sourceFrags, frags...)
	}

	profile := b.selector.ProfileFor(input.Intent)
	result := b.selector.Select(sourceFrags, profile, input.Budget)
	trace.SelectionSummary = result.Summary

	placement := b.placer.Place(result.Selected, input.Intent)
	trace.PlacementSummary = summarizePlacement(placement)

	manifest := contextfrag.BuildManifest(result.Selected)
	manifest.View = contextfrag.ManifestView(input.Intent)
	trace.Warnings = append(trace.Warnings, manifest.ValidationWarnings...)

	view := &ContextView{
		Intent:      input.Intent,
		SourceFrags: sourceFrags,
		Selected:    result.Selected,
		Placement:   placement,
		Manifest:    manifest,
		Rendered:    make(map[contextfrag.RenderTarget]RenderedPayload, len(input.Targets)),
		Trace:       trace,
	}

	if input.Options.DryRun {
		return view, nil
	}

	for _, target := range input.Targets {
		if b.renderers == nil {
			return nil, fmt.Errorf("unknown renderer %q", target)
		}
		renderer, ok := b.renderers.Get(target)
		if !ok {
			return nil, fmt.Errorf("unknown renderer %q", target)
		}
		payload, err := renderer.Render(ctx, RenderInput{
			Intent:    input.Intent,
			Selected:  result.Selected,
			Placement: placement,
			Scope:     input.Scope,
			Target:    target,
		})
		if err != nil {
			return nil, fmt.Errorf("renderer %q: %w", target, err)
		}
		if payload.Target == "" {
			payload.Target = target
		}
		view.Rendered[target] = payload
		view.Trace.RenderSummaries[target] = RenderSummary{
			Target:      payload.Target,
			ContentHash: payload.ContentHash,
			ItemCount:   len(placement.Items),
		}
	}

	return view, nil
}

func summarizePlacement(placement PlacementPlan) PlacementSummary {
	stable := placement.FirstVolatileIndex
	if stable < 0 || stable > len(placement.Items) {
		stable = len(placement.Items)
	}
	return PlacementSummary{
		StablePrefixFrags: stable,
		DynamicFrags:      len(placement.Items) - stable,
	}
}
