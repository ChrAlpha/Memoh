package contextview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

// ProviderRunConfigApplier adapts ApplyProviderRunConfig to the agent's
// injected applier hook.
func ProviderRunConfigApplier(logger *slog.Logger) agentpkg.ContextViewApplier {
	return func(ctx context.Context, cfg agentpkg.RunConfig) (agentpkg.RunConfig, error) {
		return ApplyProviderRunConfig(ctx, logger, cfg)
	}
}

// CollectProviderSourceFrags runs the provider collectors over the
// materialized RunConfig fields and returns the source fragments that become
// the first-class context carrier. A nil return means collection failed and
// the caller should stay on the legacy field path.
func CollectProviderSourceFrags(ctx context.Context, cfg agentpkg.RunConfig) []contextfrag.ContextFrag {
	systemFrags, err := (&SystemPromptCollector{}).Collect(ctx, CollectRequest{
		Scope:  cfg.ContextScope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: SystemPromptConfig{System: cfg.System, SplitWorkspace: true},
	})
	if err != nil {
		return nil
	}
	nonSystem := CollectNonSystemProviderSourceFrags(ctx, cfg)
	if nonSystem == nil {
		return nil
	}
	frags := make([]contextfrag.ContextFrag, 0, len(systemFrags)+len(nonSystem))
	frags = append(frags, systemFrags...)
	frags = append(frags, nonSystem...)
	return frags
}

// CollectNonSystemProviderSourceFrags runs the history/memory/hook/current-user/
// inline-image collectors over the materialized RunConfig fields — everything
// CollectProviderSourceFrags does except the system prompt collector — so a
// caller that builds its own system fragments some other way (e.g. directly
// from agentpkg.GenerateSystemSections) can still reuse this bundle for the
// rest. A nil return means collection failed and the caller should stay on
// the legacy field path.
func CollectNonSystemProviderSourceFrags(ctx context.Context, cfg agentpkg.RunConfig) []contextfrag.ContextFrag {
	query := cfg.Query
	inlineImages := cfg.InlineImages
	if cfg.ContextQueryMaterialized {
		query = ""
		inlineImages = nil
	}
	specs := []struct {
		collector Collector
		config    any
	}{
		{&HistoryMessagesCollector{}, HistoryMessagesConfig{
			Messages:                cfg.Messages,
			CurrentUserMessageIndex: cfg.ContextCurrentUserMessageIndex,
			TokenEstimates:          cfg.ContextHistoryTokenEstimates,
			TrimmablePrefix:         cfg.ContextTrimmableMessages,
			RepairToolClosures:      true,
		}},
		{&MemoryContextCollector{}, MemoryContextConfig{Text: cfg.ContextMemoryText}},
		{&HookContextCollector{}, HookContextConfig{Text: cfg.ContextHookText}},
		{&materializedCurrentUserCollector{}, HistoryMessagesConfig{
			Messages:                cfg.Messages,
			CurrentUserMessageIndex: cfg.ContextCurrentUserMessageIndex,
			TokenEstimates:          cfg.ContextHistoryTokenEstimates,
		}},
		{&CurrentUserCollector{}, CurrentUserConfig{Query: query}},
		{&InlineImageCollector{}, InlineImageConfig{Images: inlineImages}},
	}
	frags := make([]contextfrag.ContextFrag, 0, len(cfg.Messages)+4)
	for _, spec := range specs {
		collected, err := spec.collector.Collect(ctx, CollectRequest{
			Scope:  cfg.ContextScope,
			Intent: contextfrag.IntentRunConfigPreProvider,
			Config: spec.config,
		})
		if err != nil {
			return nil
		}
		frags = append(frags, collected...)
	}
	return frags
}

const sourceFragsCollectorName = "source_frags"

// ToolUsageFrag shapes the agent-assembled tool usage guidance as a system
// fragment that sorts between the system prompt and workspace instructions.
func ToolUsageFrag(usage string, scope contextfrag.Scope) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:            "system.tool_usage",
		Kind:          contextfrag.KindToolUsage,
		Role:          sdk.MessageRoleSystem,
		Slot:          contextfrag.SlotSystem,
		Text:          usage,
		Priority:      45,
		RetentionTier: contextfrag.RetentionPreferred,
		CacheClass:    contextfrag.CacheStable,
		Trust:         contextfrag.TrustSystem,
		Scope:         scope,
		Source:        contextfrag.SourceAgentToolUsage,
		Collector:     sourceFragsCollectorName,
		Render:        contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		ConflictKey:   toolUsageConflictKey,
	})
}

const toolUsageConflictKey = "system.tool_usage"

// DefaultRecentProtectTokens is the provider-view default for the budget
// recent-protection window: under budget pressure the newest droppable
// history within this many estimated tokens survives trimming, in line with
// the ~20K-token recency guards common across agent runtimes.
const DefaultRecentProtectTokens = 20000

func resolveRecentProtectTokens(override *int) int {
	if override == nil {
		return DefaultRecentProtectTokens
	}
	if *override < 0 {
		return 0
	}
	return *override
}

// ApplyProviderRunConfig rebuilds the provider-facing run config through the
// context view pipeline. When the run config carries source fragments they
// are the authoritative input (fragments-first); otherwise the collectors
// derive them from the materialized legacy fields. Selection, placement and
// the SDK render then produce System/Messages as outputs.
func ApplyProviderRunConfig(
	ctx context.Context,
	logger *slog.Logger,
	cfg agentpkg.RunConfig,
) (agentpkg.RunConfig, error) {
	var sources []SourceSpec
	var registry CollectorRegistry
	fragsFirst := len(cfg.ContextSourceFrags) > 0
	var providerSourceFrags []contextfrag.ContextFrag
	if fragsFirst {
		frags := append([]contextfrag.ContextFrag(nil), cfg.ContextSourceFrags...)
		if len(cfg.ContextToolUsageFrags) > 0 {
			filtered := frags[:0]
			for _, frag := range frags {
				if frag.Kind != contextfrag.KindToolUsage {
					filtered = append(filtered, frag)
				}
			}
			frags = filtered
			frags = append(frags, cfg.ContextToolUsageFrags...)
		} else if usage := strings.TrimSpace(cfg.ContextToolUsage); usage != "" {
			frags = append(frags, ToolUsageFrag(usage, cfg.ContextScope))
		}
		providerSourceFrags = frags
		registry = NewMapCollectorRegistry(StaticCollector{CollectorName: sourceFragsCollectorName, Frags: frags})
		sources = []SourceSpec{{Name: sourceFragsCollectorName}}
	} else {
		query := cfg.Query
		inlineImages := cfg.InlineImages
		if cfg.ContextQueryMaterialized {
			query = ""
			inlineImages = nil
		}
		registry = NewMapCollectorRegistry(
			&SystemPromptCollector{},
			&HistoryMessagesCollector{},
			&MemoryContextCollector{},
			&HookContextCollector{},
			&materializedCurrentUserCollector{},
			&CurrentUserCollector{},
			&InlineImageCollector{},
		)
		sources = []SourceSpec{
			{Name: "system_prompt", Config: SystemPromptConfig{System: cfg.System, ToolUsage: cfg.ContextToolUsage}},
			{Name: "history_messages", Config: HistoryMessagesConfig{
				Messages:                cfg.Messages,
				CurrentUserMessageIndex: cfg.ContextCurrentUserMessageIndex,
				TokenEstimates:          cfg.ContextHistoryTokenEstimates,
				TrimmablePrefix:         cfg.ContextTrimmableMessages,
				RepairToolClosures:      true,
			}},
			{Name: "memory_context", Config: MemoryContextConfig{Text: cfg.ContextMemoryText}},
			{Name: "hook_context", Config: HookContextConfig{Text: cfg.ContextHookText}},
			{Name: materializedCurrentUserCollectorName, Config: HistoryMessagesConfig{
				Messages:                cfg.Messages,
				CurrentUserMessageIndex: cfg.ContextCurrentUserMessageIndex,
				TokenEstimates:          cfg.ContextHistoryTokenEstimates,
			}},
			{Name: "current_user", Config: CurrentUserConfig{Query: query}},
			{Name: "inline_images", Config: InlineImageConfig{Images: inlineImages}},
		}
	}
	ledger := contextfrag.NewMutationLedger()
	budgetPlan, budgetErr := providerContextBudgetPlan(ctx, cfg)
	selector := Selector(&FragmentSelector{})
	fallbackCfg := cfg
	if fragsFirst && cfg.ContextToolDefsResolved {
		gate := newCapabilityGateSelector(selector, cfg.ContextToolDefs)
		_, gated := filterUnavailableCapabilities(providerSourceFrags, gate.available)
		if len(gated) > 0 {
			ledger.Record(contextfrag.MutationCapabilityGate, fmt.Sprintf("dropped=%d", len(gated)))
			fallbackCfg = capabilitySafeFallbackConfig(cfg, providerSourceFrags, gate.available)
		}
		selector = gate
	}
	if budgetErr != nil && !isContextBudgetError(budgetErr) {
		return providerViewFallback(logger, fallbackCfg, ledger, "budget_plan_error",
			"context budget plan failed; using legacy assembly", budgetErr), nil
	}
	builder := NewBuilder(registry, selector, StablePrefixPlacer{}, NewMapRendererRegistry(&SDKMessagesRenderer{}))
	view, err := builder.Build(ctx, BuildInput{
		Scope:   cfg.ContextScope,
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: sources,
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
		Budget: BudgetEnvelope{
			MaxTokens:           cfg.EffectiveHistoryBudgetTokens(),
			Plan:                budgetPlan,
			RecentProtectTokens: resolveRecentProtectTokens(cfg.ContextRecentProtectTokens),
			ToolExchange:        cfg.ContextToolExchangePolicy,
		},
		DynamicMutators: cfg.ContextDynamicMutators,
	})
	if budgetErr != nil {
		recordContextBudgetFailure(ledger, budgetErr)
		return providerBudgetAuditConfig(cfg, view, ledger, budgetPlan), budgetErr
	}
	if err != nil {
		if isContextBudgetError(err) {
			recordContextBudgetFailure(ledger, err)
			return providerBudgetAuditConfig(cfg, view, ledger, budgetPlan), err
		}
		return providerViewFallback(logger, fallbackCfg, ledger, "build_error",
			"context view build failed; using legacy assembly", err), nil
	}
	payload, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		return providerViewFallback(logger, fallbackCfg, ledger, "unexpected_payload",
			"context view rendered unexpected payload; using legacy assembly", nil), nil
	}

	plan := cachePlanFromPlacement(view.Placement)
	plan.StablePrefixTokenEstimate = stablePrefixTokenEstimate(view.Placement, view.Selected, cfg.ContextToolDefs)
	plan.MidStableMessageCount = midStableMessageCount(view.Placement, view.Selected)
	manifest := view.Manifest
	manifest.CachePlan = &plan
	manifest.Mutations = ledger
	manifest.ToolDefs = cfg.ContextToolDefs

	cfg.System = payload.System
	cfg.Messages = materializeRenderedQuery(payload, cfg.ContextQueryMaterialized)
	if hasCurrentUserFrag(view.Selected) {
		cfg.ContextCurrentUserMessageIndex = latestUserMessageIndex(cfg.Messages)
	}
	cfg.ContextQueryMaterialized = true
	cfg.ContextFrags = view.Selected
	cfg.ContextManifest = manifest
	cfg.ContextCachePlan = plan
	cfg.ContextMutations = ledger
	cfg.ContextStepReselector = SelectProviderStepMessages
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(manifest)
	}
	return cfg, nil
}

func providerContextBudgetPlan(
	ctx context.Context,
	cfg agentpkg.RunConfig,
) (*contextfrag.ContextBudgetPlan, error) {
	if cfg.ContextBudgetMaxTokens == 0 ||
		strings.EqualFold(strings.TrimSpace(cfg.SessionType), sessionmode.Discuss) {
		return nil, nil
	}
	currentRequestCost, err := providerCurrentRequestCost(ctx, cfg)
	if err != nil {
		return nil, err
	}
	toolDefsCost := 0
	for _, def := range cfg.ContextToolDefs {
		toolDefsCost += def.TokenEstimate
	}
	return ComputeContextBudgetPlan(
		cfg.ContextBudgetMaxTokens,
		defaultOutputReserveForWindow(cfg.ContextBudgetMaxTokens),
		toolDefsCost,
		currentRequestCost,
	)
}

func providerCurrentRequestCost(ctx context.Context, cfg agentpkg.RunConfig) (int, error) {
	if len(cfg.ContextSourceFrags) > 0 {
		return currentRequestFragCost(cfg.ContextSourceFrags), nil
	}

	query := cfg.Query
	inlineImages := cfg.InlineImages
	if cfg.ContextQueryMaterialized {
		query = ""
		inlineImages = nil
	}
	specs := []struct {
		collector Collector
		config    any
	}{
		{&materializedCurrentUserCollector{}, HistoryMessagesConfig{
			Messages:                cfg.Messages,
			CurrentUserMessageIndex: cfg.ContextCurrentUserMessageIndex,
			TokenEstimates:          cfg.ContextHistoryTokenEstimates,
		}},
		{&CurrentUserCollector{}, CurrentUserConfig{Query: query}},
		{&InlineImageCollector{}, InlineImageConfig{Images: inlineImages}},
	}
	var frags []contextfrag.ContextFrag
	for _, spec := range specs {
		collected, err := spec.collector.Collect(ctx, CollectRequest{
			Scope:  cfg.ContextScope,
			Intent: contextfrag.IntentRunConfigPreProvider,
			Config: spec.config,
		})
		if err != nil {
			return 0, err
		}
		frags = append(frags, collected...)
	}
	return currentRequestFragCost(frags), nil
}

func currentRequestFragCost(frags []contextfrag.ContextFrag) int {
	total := 0
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotCurrentUser {
			total += contextfrag.ResolveFragTokens(frag)
		}
	}
	return total
}

func isContextBudgetError(err error) bool {
	return errors.Is(err, contextfrag.ErrProtectedContextOverflow) ||
		errors.Is(err, contextfrag.ErrBudgetUnsatisfied)
}

func recordContextBudgetFailure(ledger *contextfrag.MutationLedger, err error) {
	reason := "budget_unsatisfied"
	if errors.Is(err, contextfrag.ErrProtectedContextOverflow) {
		reason = "protected_context_overflow"
	}
	ledger.Record(contextfrag.MutationContextBudgetFailure, reason)
}

func providerBudgetAuditConfig(
	cfg agentpkg.RunConfig,
	view *ContextView,
	ledger *contextfrag.MutationLedger,
	budgetPlan *contextfrag.ContextBudgetPlan,
) agentpkg.RunConfig {
	if view == nil {
		manifest := contextfrag.BuildManifest(nil)
		manifest.View = contextfrag.ViewRunConfigPreProvider
		manifest.DynamicMutators = normalizeDynamicMutators(cfg.ContextDynamicMutators)
		if budgetPlan != nil {
			plan := *budgetPlan
			manifest.BudgetPlan = &plan
		}
		manifest.Mutations = ledger
		manifest.ToolDefs = append([]contextfrag.ToolDefAccounting(nil), cfg.ContextToolDefs...)

		cfg.ContextManifest = manifest
		cfg.ContextMutations = ledger
		if cfg.ContextLifecycle != nil {
			cfg.ContextLifecycle.SetManifest(manifest)
		}
		return cfg
	}
	cachePlan := cachePlanFromPlacement(view.Placement)
	cachePlan.StablePrefixTokenEstimate = stablePrefixTokenEstimate(view.Placement, view.Selected, cfg.ContextToolDefs)
	cachePlan.MidStableMessageCount = midStableMessageCount(view.Placement, view.Selected)
	manifest := view.Manifest
	manifest.CachePlan = &cachePlan
	manifest.Mutations = ledger
	manifest.ToolDefs = cfg.ContextToolDefs

	cfg.ContextFrags = view.Selected
	cfg.ContextManifest = manifest
	cfg.ContextCachePlan = cachePlan
	cfg.ContextMutations = ledger
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(manifest)
	}
	return cfg
}

// materializeRenderedQuery closes the rendered payload into the final
// provider message stream: the current query (with its native images)
// becomes the trailing user message, and image-only turns (pipeline mode,
// where the query text already lives in the history) inject into the latest
// user message. The view owns this placement so pinned sources such as
// memory recall always precede the current request.
func materializeRenderedQuery(payload *SDKRenderedPayload, alreadyMaterialized bool) []sdk.Message {
	messages := payload.Messages
	if alreadyMaterialized {
		return messages
	}
	imageParts := make([]sdk.MessagePart, 0, len(payload.InlineImages))
	for _, img := range payload.InlineImages {
		if strings.TrimSpace(img.Image) != "" {
			imageParts = append(imageParts, img)
		}
	}
	if strings.TrimSpace(payload.Query) != "" {
		return append(messages, sdk.UserMessage(payload.Query, imageParts...))
	}
	if len(imageParts) == 0 {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == sdk.MessageRoleUser {
			messages[i].Content = append(messages[i].Content, imageParts...)
			return messages
		}
	}
	return append(messages, sdk.UserMessage("", imageParts...))
}

// providerViewFallback degrades to the legacy assembly while keeping the
// round observable: the ledger records the fallback and travels on the
// legacy-compiled manifest so the lifecycle snapshot is not lost exactly
// when the view failed.
func providerViewFallback(
	logger *slog.Logger,
	cfg agentpkg.RunConfig,
	ledger *contextfrag.MutationLedger,
	reason, message string,
	err error,
) agentpkg.RunConfig {
	warnProviderContextView(logger, cfg, message, err)
	priorManifest := cfg.ContextManifest
	out := legacyMaterializeQuery(cfg).RefreshContextFrag()
	mergeCapabilityFallbackAudit(&out, priorManifest)
	ledger.Record(contextfrag.MutationContextViewFallback, reason)
	manifest := out.ContextManifest
	manifest.Mutations = ledger
	if manifest.CachePlan == nil {
		plan := contextfrag.CachePlan{}
		manifest.CachePlan = &plan
	}
	out.ContextManifest = manifest
	out.ContextMutations = ledger
	if out.ContextLifecycle != nil {
		out.ContextLifecycle.SetManifest(manifest)
	}
	return out
}

func mergeCapabilityFallbackAudit(out *agentpkg.RunConfig, prior contextfrag.Manifest) {
	if out == nil || prior.Selection == nil ||
		prior.Selection.DropReasons[capabilityGateDropReason] == 0 {
		return
	}
	gated := make([]contextfrag.SelectionDecision, 0, prior.Selection.Dropped)
	for _, decision := range prior.SelectionDecisions {
		if decision.Decision == contextfrag.DecisionDropped &&
			decision.Reason == capabilityGateDropReason {
			gated = append(gated, decision)
		}
	}
	if len(gated) == 0 {
		return
	}
	decisions := make([]contextfrag.SelectionDecision, 0, len(out.ContextFrags)+len(gated))
	for _, frag := range out.ContextFrags {
		decisions = append(decisions, selectionDecisionForFrag(frag, contextfrag.DecisionSelected, ""))
	}
	decisions = append(decisions, gated...)
	out.ContextManifest.Selection = &contextfrag.SelectionTrace{
		Selected: len(out.ContextFrags),
		Dropped:  len(gated),
		DropReasons: map[string]int{
			capabilityGateDropReason: len(gated),
		},
	}
	out.ContextManifest.SelectionDecisions = decisions
}

// legacyMaterializeQuery reproduces the pre-view query placement for the
// build-error fallback, so a broken view degrades to the legacy assembly
// instead of silently dropping the current request and memory recall.
func legacyMaterializeQuery(cfg agentpkg.RunConfig) agentpkg.RunConfig {
	if usage := strings.TrimSpace(cfg.ContextToolUsage); usage != "" && len(cfg.ContextSourceFrags) > 0 &&
		!strings.Contains(cfg.System, usage) {
		if idx := strings.Index(cfg.System, contextfrag.WorkspaceInstructionAnchor); idx >= 0 {
			cfg.System = strings.TrimSpace(cfg.System[:idx]) + "\n\n" + usage + "\n" + cfg.System[idx:]
		} else {
			cfg.System = strings.TrimSpace(cfg.System + "\n\n" + usage)
		}
	}
	dynamicMessages := make([]sdk.Message, 0, 2)
	if raw := strings.TrimSpace(cfg.ContextMemoryText); raw != "" {
		if text := fallbackMemoryContext(raw); text != "" {
			dynamicMessages = append(dynamicMessages, sdk.UserMessage(text))
		}
		cfg.ContextMemoryText = ""
	}
	if raw := strings.TrimSpace(cfg.ContextHookText); raw != "" {
		if text := fallbackHookContext(raw); text != "" {
			dynamicMessages = append(dynamicMessages, sdk.UserMessage(text))
		}
	}
	cfg.ContextHookText = ""
	cfg = insertDynamicContextBeforeCurrentUser(cfg, dynamicMessages)
	if cfg.ContextQueryMaterialized {
		return cfg
	}
	imageParts := make([]sdk.MessagePart, 0, len(cfg.InlineImages))
	for _, img := range cfg.InlineImages {
		if strings.TrimSpace(img.Image) != "" {
			imageParts = append(imageParts, img)
		}
	}
	switch {
	case strings.TrimSpace(cfg.Query) != "":
		cfg.Messages = append(cfg.Messages, sdk.UserMessage(cfg.Query, imageParts...))
		index := len(cfg.Messages) - 1
		cfg.ContextCurrentUserMessageIndex = &index
	case len(imageParts) > 0:
		injected := false
		for i := len(cfg.Messages) - 1; i >= 0; i-- {
			if cfg.Messages[i].Role == sdk.MessageRoleUser {
				cfg.Messages[i].Content = append(cfg.Messages[i].Content, imageParts...)
				injected = true
				break
			}
		}
		if !injected {
			cfg.Messages = append(cfg.Messages, sdk.UserMessage("", imageParts...))
			index := len(cfg.Messages) - 1
			cfg.ContextCurrentUserMessageIndex = &index
		}
	}
	cfg.ContextQueryMaterialized = true
	return cfg
}

func insertDynamicContextBeforeCurrentUser(cfg agentpkg.RunConfig, dynamic []sdk.Message) agentpkg.RunConfig {
	if len(dynamic) == 0 {
		return cfg
	}
	currentIndex, ok := markedCurrentUserMessageIndex(cfg.Messages, cfg.ContextCurrentUserMessageIndex)
	if !ok {
		cfg.Messages = append(cfg.Messages, dynamic...)
		return cfg
	}
	messages := make([]sdk.Message, 0, len(cfg.Messages)+len(dynamic))
	messages = append(messages, cfg.Messages[:currentIndex]...)
	messages = append(messages, dynamic...)
	messages = append(messages, cfg.Messages[currentIndex:]...)
	cfg.Messages = messages
	currentIndex += len(dynamic)
	cfg.ContextCurrentUserMessageIndex = &currentIndex
	return cfg
}

func markedCurrentUserMessageIndex(messages []sdk.Message, index *int) (int, bool) {
	if index == nil {
		return 0, false
	}
	if *index >= 0 && *index < len(messages) && messages[*index].Role == sdk.MessageRoleUser {
		return *index, true
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == sdk.MessageRoleUser {
			return i, true
		}
	}
	return 0, false
}

func latestUserMessageIndex(messages []sdk.Message) *int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == sdk.MessageRoleUser {
			index := i
			return &index
		}
	}
	return nil
}

func hasCurrentUserFrag(frags []contextfrag.ContextFrag) bool {
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotCurrentUser {
			return true
		}
	}
	return false
}

func fallbackMemoryContext(text string) string {
	formatted := FormatMemoryContext(text)
	if utf8.RuneCountInString(formatted) > maxMemoryContextChars {
		return ""
	}
	return formatted
}

func fallbackHookContext(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > maxHookContextChars {
		return ""
	}
	return text
}

// stablePrefixTokenEstimate measures everything the message-level cache
// breakpoint covers: the tool roster, stable system-slot fragments (they
// render into the system prompt ahead of history), and the stable leading
// messages. Recorded on the plan so cache reads can be judged against the
// prefix quality that was actually on offer.
func stablePrefixTokenEstimate(placement PlacementPlan, selected []contextfrag.ContextFrag, toolDefs []contextfrag.ToolDefAccounting) int {
	total := 0
	for _, def := range toolDefs {
		total += def.TokenEstimate
	}
	if len(placement.Items) == 0 {
		return total
	}
	byID := make(map[string]contextfrag.ContextFrag, len(selected))
	for _, frag := range selected {
		byID[frag.ID] = frag
	}
	for i, item := range placement.Items {
		if i >= placement.FirstVolatileIndex {
			break
		}
		if frag, ok := byID[item.FragID]; ok {
			total += contextfrag.ResolveFragTokens(frag)
		}
	}
	return total
}

// midBreakpointMinSpanTokens gates the extra mid-span breakpoint: below
// twice Anthropic's nominal minimum cacheable prefix, losing the tail costs
// little and the extra cache entry is not worth its write.
const midBreakpointMinSpanTokens = 2048

// midStableMessageCount picks where the insurance breakpoint goes inside a
// large stable message span: the smallest leading message count holding at
// least half the span's token mass. Zero means the span is too small, or
// the midpoint would duplicate the tail breakpoint.
func midStableMessageCount(placement PlacementPlan, selected []contextfrag.ContextFrag) int {
	byID := make(map[string]contextfrag.ContextFrag, len(selected))
	for _, frag := range selected {
		byID[frag.ID] = frag
	}
	var perMessage []int
	total := 0
	for i, item := range placement.Items {
		if i >= placement.FirstVolatileIndex {
			break
		}
		if item.Slot == contextfrag.SlotSystem {
			continue
		}
		tokens := 0
		if frag, ok := byID[item.FragID]; ok {
			tokens = contextfrag.ResolveFragTokens(frag)
		}
		perMessage = append(perMessage, tokens)
		total += tokens
	}
	if total < midBreakpointMinSpanTokens {
		return 0
	}
	cumulative := 0
	for i, tokens := range perMessage {
		cumulative += tokens
		if cumulative*2 >= total {
			count := i + 1
			if count >= len(perMessage) {
				return 0
			}
			return count
		}
	}
	return 0
}

// cachePlanFromPlacement projects the placement plan onto the rendered
// message stream: system-slot fragments render into the system prompt, so
// only non-system fragments inside the stable prefix count toward the
// message-level cache breakpoint.
func cachePlanFromPlacement(placement PlacementPlan) contextfrag.CachePlan {
	stableMessages := 0
	for i, item := range placement.Items {
		if i >= placement.FirstVolatileIndex {
			break
		}
		if item.Slot != contextfrag.SlotSystem {
			stableMessages++
		}
	}
	return contextfrag.CachePlan{
		StablePrefixHash:   placement.StablePrefixHash,
		StableMessageCount: stableMessages,
	}
}

func warnProviderContextView(logger *slog.Logger, cfg agentpkg.RunConfig, msg string, err error) {
	if logger == nil {
		return
	}
	attrs := []any{
		slog.String("bot_id", cfg.Identity.BotID),
		slog.String("session_id", cfg.Identity.SessionID),
	}
	if err != nil {
		attrs = append(attrs, slog.Any("error", err))
	}
	logger.Warn(msg, attrs...) //nolint:sloglint // caller-provided audit message
}
