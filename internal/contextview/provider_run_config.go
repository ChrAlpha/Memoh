package contextview

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

// ProviderRunConfigApplier adapts ApplyProviderRunConfig to the agent's
// injected applier hook.
func ProviderRunConfigApplier(logger *slog.Logger) agentpkg.ContextViewApplier {
	return func(ctx context.Context, cfg agentpkg.RunConfig) agentpkg.RunConfig {
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
func ApplyProviderRunConfig(ctx context.Context, logger *slog.Logger, cfg agentpkg.RunConfig) agentpkg.RunConfig {
	var sources []SourceSpec
	var registry CollectorRegistry
	if len(cfg.ContextSourceFrags) > 0 {
		frags := append([]contextfrag.ContextFrag(nil), cfg.ContextSourceFrags...)
		if usage := strings.TrimSpace(cfg.ContextToolUsage); usage != "" {
			frags = append(frags, ToolUsageFrag(usage, cfg.ContextScope))
		}
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
	builder := NewBuilder(registry, &FragmentSelector{}, StablePrefixPlacer{}, NewMapRendererRegistry(&SDKMessagesRenderer{}))
	view, err := builder.Build(ctx, BuildInput{
		Scope:   cfg.ContextScope,
		Intent:  contextfrag.IntentRunConfigPreProvider,
		Sources: sources,
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
		Budget: BudgetEnvelope{
			MaxTokens:           cfg.EffectiveHistoryBudgetTokens(),
			RecentProtectTokens: resolveRecentProtectTokens(cfg.ContextRecentProtectTokens),
			ToolExchange:        cfg.ContextToolExchangePolicy,
		},
		DynamicMutators: cfg.ContextDynamicMutators,
	})
	if err != nil {
		return providerViewFallback(logger, cfg, ledger, "build_error",
			"context view build failed; using legacy assembly", err)
	}
	payload, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		return providerViewFallback(logger, cfg, ledger, "unexpected_payload",
			"context view rendered unexpected payload; using legacy assembly", nil)
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
	out := legacyMaterializeQuery(cfg).RefreshContextFrag()
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
