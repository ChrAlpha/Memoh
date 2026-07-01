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

func (b *Builder) Build(ctx context.Context, input BuildInput) (ContextView, error) {
	if b.collectors == nil {
		return ContextView{}, errors.New("collector registry is required")
	}
	if b.selector == nil {
		return ContextView{}, errors.New("selector is required")
	}
	if b.placer == nil {
		return ContextView{}, errors.New("placer is required")
	}

	trace := BuildTrace{CollectDurations: make(map[string]int64, len(input.Sources))}
	collected := make([]contextfrag.ContextFrag, 0)
	for _, source := range input.Sources {
		name := strings.TrimSpace(source.Name)
		collector, ok := b.collectors.Collector(name)
		if !ok {
			return ContextView{Trace: trace}, fmt.Errorf("unknown collector %q", name)
		}

		start := time.Now()
		frags, err := collector.Collect(ctx, input, source)
		trace.CollectDurations[name] = time.Since(start).Microseconds()
		if err != nil {
			return ContextView{Trace: trace}, fmt.Errorf("collector %q: %w", name, err)
		}
		collected = append(collected, frags...)
	}

	profile := b.selector.ProfileFor(input.Intent)
	if profile.Intent == "" {
		profile.Intent = input.Intent
	}
	if profile.View == "" {
		profile.View = manifestViewForIntent(input.Intent)
	}
	if profile.Budget == (BudgetEnvelope{}) {
		profile.Budget = input.Budget
	}

	selection, err := b.selector.Select(ctx, profile, input, collected)
	if err != nil {
		return ContextView{Trace: trace}, fmt.Errorf("select: %w", err)
	}
	trace.Selection = selection.Summary

	placement, err := b.placer.Place(ctx, profile, input, selection.Frags)
	if err != nil {
		return ContextView{Trace: trace}, fmt.Errorf("place: %w", err)
	}
	trace.Placement = placement.Summary

	manifest := contextfrag.BuildManifest(selection.Frags)
	manifest.View = profile.View
	view := ContextView{
		Intent:    profile.Intent,
		Profile:   profile,
		Frags:     selection.Frags,
		Manifest:  manifest,
		Placement: placement,
		Trace:     trace,
		Warnings:  append([]string(nil), selection.Warnings...),
	}

	if input.Options.DryRun {
		return view, nil
	}

	for _, target := range requestedRenderTargets(input, profile) {
		if b.renderers == nil {
			return view, fmt.Errorf("unknown renderer %q", target)
		}
		renderer, ok := b.renderers.Renderer(target)
		if !ok {
			return view, fmt.Errorf("unknown renderer %q", target)
		}
		payload, err := renderer.Render(ctx, target, input, view)
		if err != nil {
			return view, fmt.Errorf("renderer %q: %w", target, err)
		}
		if payload.Target == "" {
			payload.Target = target
		}
		view.Rendered = append(view.Rendered, payload)
		view.Trace.Render = append(view.Trace.Render, RenderSummary{
			Target:      payload.Target,
			ContentHash: payload.ContentHash,
			ItemCount:   payload.ItemCount,
		})
	}

	return view, nil
}

func manifestViewForIntent(intent contextfrag.Intent) contextfrag.ManifestView {
	switch intent {
	case contextfrag.IntentCompactionCandidates:
		return contextfrag.ViewCompactionCandidates
	case contextfrag.IntentDiscussReply:
		return contextfrag.ViewDiscussReply
	case contextfrag.IntentACPRuntimePrompt:
		return contextfrag.ViewACPRuntimePrompt
	default:
		return contextfrag.ViewRunConfigPreProvider
	}
}

func requestedRenderTargets(input BuildInput, profile IntentProfile) []contextfrag.RenderTarget {
	if len(input.RenderTargets) > 0 {
		return input.RenderTargets
	}
	return profile.RenderTargets
}
