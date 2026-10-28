package contextview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
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
		collector, ok := b.collectors.Get(spec.Name)
		if !ok {
			return nil, fmt.Errorf("unknown collector %q", spec.Name)
		}

		start := time.Now()
		frags, err := collector.Collect(ctx, CollectRequest{
			Scope:  input.Scope,
			Intent: input.Intent,
			Config: spec.Config,
		})
		trace.CollectDurations[spec.Name] = time.Since(start).Microseconds()
		if err != nil {
			return nil, fmt.Errorf("collector %q: %w", spec.Name, err)
		}
		sourceFrags = append(sourceFrags, frags...)
	}
	// Provider system fragments render into one leading system field even when
	// a late collector (for example tool usage) supplied them after message
	// fragments. Canonicalize that order before selection and placement so the
	// cache plan describes the same prefix the renderer emits.
	sourceFrags = sortSystemFragsByPriority(sourceFrags)
	sourceFrags = distinctContextRefs(contextfrag.NormalizeContextRefs(sourceFrags))

	profile := b.selector.ProfileFor(input.Intent)
	result := b.selector.Select(sourceFrags, profile, input.Budget)
	trace.SelectionSummary = result.Summary
	if result.TrimNotice && result.TrimNoticeIndex >= 0 && result.TrimNoticeIndex <= len(result.Selected) {
		notice := contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{TrimNoticeFrag(input.Scope)})[0]
		selected := make([]contextfrag.ContextFrag, 0, len(result.Selected)+1)
		selected = append(selected, result.Selected[:result.TrimNoticeIndex]...)
		selected = append(selected, notice)
		selected = append(selected, result.Selected[result.TrimNoticeIndex:]...)
		result.Selected = selected
	}

	placement := b.placer.Place(result.Selected, input.Intent)
	trace.PlacementSummary = summarizePlacement(placement)

	manifest := contextfrag.BuildManifest(result.Selected)
	manifest.View = input.Intent.ManifestView()
	manifest.DynamicMutators = normalizeDynamicMutators(input.DynamicMutators)
	manifest.Mutations = input.Mutations
	manifest.Selection = selectionTrace(result.Summary)
	manifest.SelectionDecisions = selectionDecisions(sourceFrags, result)
	if input.Budget.Plan != nil {
		plan := *input.Budget.Plan
		manifest.BudgetPlan = &plan
	}
	manifest.EditTrace = append(manifest.EditTrace, selectionEditTrace(result.Dropped)...)
	manifest.EditTrace = append(manifest.EditTrace, result.Edited...)
	manifest.ValidationWarnings = append(manifest.ValidationWarnings, result.Warnings...)
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

	if result.FatalError != nil {
		return view, result.FatalError
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
			Manifest:  &manifest,
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
			Target:    payload.Target,
			ItemCount: len(placement.Items),
		}
	}

	return view, nil
}

// selectionDecisions names what selection did with each source fragment:
// dropped (with the reason), kept as is, or kept as the selector edited it.
// Fragments are matched by ref identity, in order; a selector that recorded
// a drop without a ref is matched by fragment ID. What selection added (a
// marker or notice) follows as selected.
func selectionDecisions(sourceFrags []contextfrag.ContextFrag, result SelectionResult) []contextfrag.SelectionDecision {
	droppedByRef := make(map[string][]string, len(result.Summary.DropReasons))
	droppedByID := make(map[string][]string)
	for _, record := range result.Summary.DropReasons {
		if key, ok := selectionRefKey(record.Ref); ok {
			droppedByRef[key] = append(droppedByRef[key], record.Reason)
			continue
		}
		droppedByID[record.FragID] = append(droppedByID[record.FragID], record.Reason)
	}
	selectedByRef := make(map[string][]int, len(result.Selected))
	selectedByID := make(map[string][]int, len(result.Selected))
	for i, frag := range result.Selected {
		if key, ok := selectionRefKey(frag.Ref); ok {
			selectedByRef[key] = append(selectedByRef[key], i)
		}
		selectedByID[frag.ID] = append(selectedByID[frag.ID], i)
	}
	selectedUsed := make([]bool, len(result.Selected))
	take := func(queue map[string][]int, key string) (int, bool) {
		indexes := queue[key]
		for len(indexes) > 0 && selectedUsed[indexes[0]] {
			indexes = indexes[1:]
		}
		queue[key] = indexes
		if len(indexes) == 0 {
			return 0, false
		}
		selectedUsed[indexes[0]] = true
		return indexes[0], true
	}

	decisions := make([]contextfrag.SelectionDecision, 0, len(sourceFrags)+len(result.Selected))
	for _, source := range sourceFrags {
		key, hasRef := selectionRefKey(source.Ref)
		if reasons := droppedByRef[key]; hasRef && len(reasons) > 0 {
			decisions = append(decisions, selectionDecisionForFrag(source, contextfrag.DecisionDropped, reasons[0]))
			droppedByRef[key] = reasons[1:]
			continue
		}
		if reasons := droppedByID[source.ID]; len(reasons) > 0 {
			decisions = append(decisions, selectionDecisionForFrag(source, contextfrag.DecisionDropped, reasons[0]))
			droppedByID[source.ID] = reasons[1:]
			continue
		}
		index, ok := 0, false
		if hasRef {
			index, ok = take(selectedByRef, key)
		}
		if !ok {
			index, ok = take(selectedByID, source.ID)
		}
		if !ok {
			decisions = append(decisions, selectionDecisionForFrag(source, contextfrag.DecisionDropped, "unknown"))
			continue
		}
		decision, reason := contextfrag.DecisionSelected, result.EditReasons[source.ID]
		if reason != "" {
			decision = contextfrag.DecisionTrimmed
		}
		decisions = append(decisions, selectionDecisionForFrag(result.Selected[index], decision, reason))
	}
	for i, selected := range result.Selected {
		if !selectedUsed[i] {
			reason := ""
			if selected.ID == systemBudgetMarkerID {
				reason = "system_budget_marker"
			}
			decisions = append(decisions, selectionDecisionForFrag(selected, contextfrag.DecisionSelected, reason))
		}
	}
	return decisions
}

// distinctContextRefs makes each source's ref name it alone: a repeated
// identity (two fragments of one source, or source-less fragments sharing
// an ID) gets its occurrence as an ordinal, so selection decisions pair each
// source with what became of it.
func distinctContextRefs(frags []contextfrag.ContextFrag) []contextfrag.ContextFrag {
	seen := make(map[string]int, len(frags))
	for i := range frags {
		key := frags[i].Ref.StableKey()
		if n := seen[key]; n > 0 {
			frags[i].Ref.ID = fmt.Sprintf("%s#%d", frags[i].Ref.ID, n)
		}
		seen[key]++
	}
	return frags
}

// selectionRefKey is the identity of a fragment's ref, if it names one.
func selectionRefKey(ref contextfrag.ContextRef) (string, bool) {
	if strings.TrimSpace(ref.Namespace) == "" || strings.TrimSpace(ref.ID) == "" {
		return "", false
	}
	return ref.StableKey(), true
}

func selectionDecisionForFrag(
	frag contextfrag.ContextFrag,
	decision contextfrag.SelectionDecisionKind,
	reason string,
) contextfrag.SelectionDecision {
	item := contextfrag.NewManifestItem(frag)
	return contextfrag.SelectionDecision{
		ID:            frag.ID,
		Ref:           item.Ref,
		Slot:          frag.Slot,
		Source:        frag.Provenance.Source,
		SourceID:      frag.Provenance.SourceID,
		Decision:      decision,
		Reason:        reason,
		TokenEstimate: item.TokenEstimate,
		TextBytes:     item.TextBytes,
		ImageCount:    item.ImageCount,
		CacheClass:    frag.CacheClass,
		RetentionTier: frag.RetentionTier,
	}
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

func normalizeDynamicMutators(mutators []contextfrag.DynamicMutator) []contextfrag.DynamicMutator {
	if len(mutators) == 0 {
		return nil
	}
	out := make([]contextfrag.DynamicMutator, 0, len(mutators))
	seen := make(map[contextfrag.DynamicMutator]bool, len(mutators))
	for _, mutator := range mutators {
		if mutator == "" || seen[mutator] {
			continue
		}
		seen[mutator] = true
		out = append(out, mutator)
	}
	return out
}

func selectionTrace(summary SelectionSummary) *contextfrag.SelectionTrace {
	if summary.TotalCollected == 0 && summary.TotalSelected == 0 && summary.TotalDropped == 0 && len(summary.DropReasons) == 0 {
		return nil
	}
	trace := &contextfrag.SelectionTrace{
		Selected: summary.TotalSelected,
		Dropped:  summary.TotalDropped,
	}
	if len(summary.DropReasons) > 0 {
		trace.DropReasons = make(map[string]int, len(summary.DropReasons))
		for _, record := range summary.DropReasons {
			reason := record.Reason
			if reason == "" {
				reason = "unknown"
			}
			trace.DropReasons[reason]++
		}
	}
	return trace
}

func selectionEditTrace(dropped []contextfrag.ContextFrag) []contextfrag.ContextEditTrace {
	if len(dropped) == 0 {
		return nil
	}
	out := make([]contextfrag.ContextEditTrace, 0, len(dropped))
	for _, frag := range dropped {
		ref := frag.Ref
		if err := contextfrag.ValidateContextRef(ref); err != nil {
			ref = contextfrag.WithContextRef(frag, ref).Ref
		}
		out = append(out, contextfrag.ContextEditTrace{
			EditID: "selection.drop." + frag.ID,
			Op:     contextfrag.EditRemove,
			Slot:   frag.Slot,
			Refs:   []contextfrag.ContextRef{ref},
		})
	}
	return out
}
