package contextview

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

const (
	systemBudgetDropReason = "system_budget"
	systemBudgetMarkerID   = "system.budget_notice"

	systemBudgetMarkerMaxBytes   = 512
	systemBudgetMarkerIDMaxBytes = 80
)

// ComputeContextBudgetPlan allocates the fixed reserves named by the provider
// envelope contract. outputReserve is the resolved generation limit, so the
// plan reserves exactly what the provider request may emit. Reserves that
// leave nothing for the system prompt are not a local failure: the system
// budget is zero, the system pass drops what its retention tiers allow, and
// the request goes to the provider, whose limit error is the outcome.
func ComputeContextBudgetPlan(window, outputReserve, toolDefsCost, currentRequestCost int) *contextfrag.ContextBudgetPlan {
	if window <= 0 {
		return nil
	}
	return &contextfrag.ContextBudgetPlan{
		Estimator:                    contextfrag.ProviderBudgetEstimator,
		EstimatorSafetyFactorPercent: contextfrag.ProviderBudgetSafetyFactorPercent,
		Window:                       window,
		OutputReserve:                outputReserve,
		ToolDefsCost:                 toolDefsCost,
		CurrentRequestCost:           currentRequestCost,
		SystemBudget:                 max(window-outputReserve-toolDefsCost-currentRequestCost, 0),
	}
}

// PlanOverflows reports whether the request a plan describes is larger than
// the window it was planned for: the system sections the retention tiers keep
// do not fit beside the reserves.
func PlanOverflows(plan *contextfrag.ContextBudgetPlan) bool {
	return plan != nil && plan.ActualSystemCost > plan.SystemBudget
}

type systemBudgetCandidate struct {
	index int
	frag  contextfrag.ContextFrag
}

// enforceSystemBudget drops optional, then preferred, system sections until
// the rest fits the plan's system budget. Required sections are never
// dropped; when they alone do not fit, the selection keeps them and the plan
// records the overflow (PlanOverflows).
func enforceSystemBudget(
	frags []contextfrag.ContextFrag,
	profile IntentProfile,
	plan *contextfrag.ContextBudgetPlan,
	fragBudgetDropped []fragBudgetDrop,
) ([]contextfrag.ContextFrag, []contextfrag.ContextFrag) {
	if !systemBudgetPlanActive(profile, plan) {
		return frags, nil
	}

	droppedIDs := make([]string, 0, len(fragBudgetDropped))
	scope := firstSystemScope(frags)
	hasScope := false
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem {
			hasScope = true
			break
		}
	}
	for _, drop := range fragBudgetDropped {
		if drop.frag.Slot != contextfrag.SlotSystem {
			continue
		}
		droppedIDs = append(droppedIDs, drop.frag.ID)
		if !hasScope {
			scope = drop.frag.Scope
			hasScope = true
		}
	}
	droppedIndexes := make(map[int]bool)
	dropped := make([]contextfrag.ContextFrag, 0)
	for _, header := range sweepEmptySystemGroupHeaders(frags, droppedIndexes) {
		droppedIndexes[header.index] = true
		dropped = append(dropped, header.frag)
		droppedIDs = append(droppedIDs, header.frag.ID)
	}
	var marker contextfrag.ContextFrag
	if len(droppedIDs) > 0 {
		marker = systemBudgetMarkerFrag(droppedIDs, scope)
	}
	selected := systemBudgetSelection(frags, droppedIndexes, marker)
	total := systemFragCost(selected)
	if total <= plan.SystemBudget {
		finishSystemBudgetPlan(plan, total)
		return selected, dropped
	}

	candidates := make([]systemBudgetCandidate, 0)
	for i, frag := range frags {
		if frag.Slot != contextfrag.SlotSystem ||
			isRenderGroupHeader(frag) ||
			frag.Budget.Overflow == contextfrag.OverflowKeep ||
			(frag.RetentionTier != contextfrag.RetentionOptional && frag.RetentionTier != contextfrag.RetentionPreferred) {
			continue
		}
		candidates = append(candidates, systemBudgetCandidate{index: i, frag: frag})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i].frag, candidates[j].frag
		if left.RetentionTier != right.RetentionTier {
			return left.RetentionTier == contextfrag.RetentionOptional
		}
		if left.DropPriority != right.DropPriority {
			return left.DropPriority.DropsBefore(right.DropPriority)
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return left.ID < right.ID
	})

	for _, candidate := range candidates {
		if droppedIndexes[candidate.index] {
			continue
		}
		droppedIndexes[candidate.index] = true
		dropped = append(dropped, candidate.frag)
		droppedIDs = append(droppedIDs, candidate.frag.ID)
		for _, header := range sweepEmptySystemGroupHeaders(frags, droppedIndexes) {
			droppedIndexes[header.index] = true
			dropped = append(dropped, header.frag)
			droppedIDs = append(droppedIDs, header.frag.ID)
		}
		marker = systemBudgetMarkerFrag(droppedIDs, scope)
		selected = systemBudgetSelection(frags, droppedIndexes, marker)
		actual := systemFragCost(selected)
		if actual <= plan.SystemBudget {
			finishSystemBudgetPlan(plan, actual)
			return selected, dropped
		}
	}

	if len(droppedIDs) > 0 {
		marker = systemBudgetMarkerFrag(droppedIDs, scope)
	}
	selected = systemBudgetSelection(frags, droppedIndexes, marker)
	actual := systemFragCost(selected)
	finishSystemBudgetPlan(plan, actual)
	return selected, dropped
}

func systemBudgetPlanActive(profile IntentProfile, plan *contextfrag.ContextBudgetPlan) bool {
	return plan != nil &&
		(profile.Intent == contextfrag.IntentRunConfigPreProvider || profile.Intent == contextfrag.IntentDiscussReply)
}

func finishSystemBudgetPlan(plan *contextfrag.ContextBudgetPlan, actual int) {
	plan.ActualSystemCost = actual
	plan.HistoryBudget = max(plan.SystemBudget-actual, 0)
}

func systemFragCost(frags []contextfrag.ContextFrag) int {
	frags = sortSystemFragsByPriority(frags)
	resolved := 0
	renderedBytes := 0
	count := 0
	var previousRender contextfrag.RenderPolicy
	for _, frag := range frags {
		if frag.Slot != contextfrag.SlotSystem {
			continue
		}
		if count > 0 {
			renderedBytes += len(contextfrag.RenderSeparator(previousRender, frag.Render))
		}
		for _, part := range frag.Parts {
			if part.Type == contextfrag.PartText {
				renderedBytes += len(contextfrag.RenderText(part.Text, frag.Render))
			}
		}
		resolved += contextfrag.ResolveFragTokens(frag)
		previousRender = frag.Render
		count++
	}
	// Preserve authoritative per-fragment estimates while also measuring the
	// exact rendered byte stream. The latter closes integer-flooring gaps
	// across short sections and their "\n\n" boundaries.
	conservative := resolved
	if count > 1 {
		conservative += count - 1
	}
	if rendered := contextfrag.ProviderBudgetTokensFromBytes(renderedBytes); rendered > conservative {
		return rendered
	}
	return conservative
}

func sweepEmptySystemGroupHeaders(
	frags []contextfrag.ContextFrag,
	droppedIndexes map[int]bool,
) []systemBudgetCandidate {
	itemCounts := make(map[string]int)
	headers := make(map[string]systemBudgetCandidate)
	for i, frag := range frags {
		if droppedIndexes[i] ||
			frag.Slot != contextfrag.SlotSystem ||
			frag.Render.GroupID == "" {
			continue
		}
		if isRenderGroupHeader(frag) {
			headers[frag.Render.GroupID] = systemBudgetCandidate{index: i, frag: frag}
			continue
		}
		itemCounts[frag.Render.GroupID]++
	}
	var swept []systemBudgetCandidate
	for groupID, header := range headers {
		if itemCounts[groupID] == 0 {
			swept = append(swept, header)
		}
	}
	sort.Slice(swept, func(i, j int) bool {
		return swept[i].frag.ID < swept[j].frag.ID
	})
	return swept
}

func firstSystemScope(frags []contextfrag.ContextFrag) contextfrag.Scope {
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem {
			return frag.Scope
		}
	}
	return contextfrag.Scope{}
}

func systemBudgetSelection(
	frags []contextfrag.ContextFrag,
	droppedIndexes map[int]bool,
	marker contextfrag.ContextFrag,
) []contextfrag.ContextFrag {
	kept := make([]contextfrag.ContextFrag, 0, len(frags)-len(droppedIndexes)+1)
	lastSystem := -1
	for i, frag := range frags {
		if droppedIndexes[i] {
			continue
		}
		kept = append(kept, frag)
		if frag.Slot == contextfrag.SlotSystem {
			lastSystem = len(kept) - 1
		}
	}
	if marker.ID == "" {
		return kept
	}
	insertAt := lastSystem + 1
	kept = append(kept, contextfrag.ContextFrag{})
	copy(kept[insertAt+1:], kept[insertAt:])
	kept[insertAt] = marker
	return kept
}

func systemBudgetMarkerFrag(droppedIDs []string, scope contextfrag.Scope) contextfrag.ContextFrag {
	frag := contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:            systemBudgetMarkerID,
		Kind:          contextfrag.KindSystemPolicy,
		Role:          sdk.MessageRoleSystem,
		Slot:          contextfrag.SlotSystem,
		Text:          systemBudgetMarkerText(droppedIDs),
		Priority:      int(^uint(0) >> 1),
		RetentionTier: contextfrag.RetentionRequired,
		CacheClass:    contextfrag.CacheDynamic,
		Trust:         contextfrag.TrustSystem,
		Scope:         scope,
		Source:        contextfrag.SourceRunConfig,
		Collector:     "system_budget",
		Render:        contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		Budget:        contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
	return contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{frag})[0]
}

func systemBudgetMarkerText(droppedIDs []string) string {
	const prefix = "[System Notice] Some system sections were omitted to fit the context window: "
	if len(droppedIDs) == 0 {
		return prefix + "details unavailable."
	}
	var listed []string
	for i, id := range droppedIDs {
		displayID := systemBudgetMarkerDisplayID(id)
		candidateIDs := displayID
		if len(listed) > 0 {
			candidateIDs = strings.Join(listed, ", ") + ", " + displayID
		}
		candidate := prefix + candidateIDs + systemBudgetMarkerTail(len(droppedIDs)-i-1)
		if len(candidate) > systemBudgetMarkerMaxBytes {
			break
		}
		listed = append(listed, displayID)
	}
	if len(listed) == 0 {
		return prefix + strconv.Itoa(len(droppedIDs)) + " section IDs not shown."
	}
	return prefix + strings.Join(listed, ", ") + systemBudgetMarkerTail(len(droppedIDs)-len(listed))
}

func systemBudgetMarkerTail(omitted int) string {
	if omitted <= 0 {
		return "."
	}
	return ", ... (+" + strconv.Itoa(omitted) + " more)."
}

func systemBudgetMarkerDisplayID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unnamed"
	}
	var token strings.Builder
	token.Grow(min(len(raw), systemBudgetMarkerIDMaxBytes))
	changed := false
	for i := 0; i < len(raw); i++ {
		value := raw[i]
		if !systemBudgetMarkerIDByte(value) {
			value = '-'
			changed = true
		}
		if token.Len() < systemBudgetMarkerIDMaxBytes {
			token.WriteByte(value)
		} else {
			changed = true
		}
	}
	value := token.String()
	if !changed {
		return value
	}
	sum := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(sum[:4])
	prefixLimit := systemBudgetMarkerIDMaxBytes - len(suffix) - 1
	if len(value) > prefixLimit {
		value = value[:prefixLimit]
	}
	value = strings.TrimRight(value, "-._:")
	if value == "" {
		value = "section"
	}
	return value + "-" + suffix
}

func systemBudgetMarkerIDByte(value byte) bool {
	return (value >= 'a' && value <= 'z') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= '0' && value <= '9') ||
		value == '-' || value == '_' || value == '.' || value == ':'
}
