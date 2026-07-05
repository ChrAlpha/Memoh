package contextview

import (
	"context"
	"log/slog"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
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
		{&SystemPromptCollector{}, SystemPromptConfig{System: cfg.System, SplitWorkspace: true}},
		{&HistoryMessagesCollector{}, HistoryMessagesConfig{
			Messages:           cfg.Messages,
			TokenEstimates:     cfg.ContextHistoryTokenEstimates,
			TrimmablePrefix:    cfg.ContextTrimmableMessages,
			RepairToolClosures: true,
		}},
		{&MemoryContextCollector{}, MemoryContextConfig{Text: cfg.ContextMemoryText}},
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
		ID:          "system.tool_usage",
		Kind:        contextfrag.KindToolUsage,
		Role:        sdk.MessageRoleSystem,
		Slot:        contextfrag.SlotSystem,
		Text:        usage,
		Priority:    45,
		CacheClass:  contextfrag.CacheStable,
		Trust:       contextfrag.TrustSystem,
		Scope:       scope,
		Source:      contextfrag.SourceAgentToolUsage,
		Collector:   sourceFragsCollectorName,
		Render:      contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
		ConflictKey: toolUsageConflictKey,
	})
}

const toolUsageConflictKey = "system.tool_usage"

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
			&CurrentUserCollector{},
			&InlineImageCollector{},
		)
		sources = []SourceSpec{
			{Name: "system_prompt", Config: SystemPromptConfig{System: cfg.System, ToolUsage: cfg.ContextToolUsage}},
			{Name: "history_messages", Config: HistoryMessagesConfig{
				Messages:           cfg.Messages,
				TokenEstimates:     cfg.ContextHistoryTokenEstimates,
				TrimmablePrefix:    cfg.ContextTrimmableMessages,
				RepairToolClosures: true,
			}},
			{Name: "memory_context", Config: MemoryContextConfig{Text: cfg.ContextMemoryText}},
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
			MaxTokens:    cfg.ContextBudgetMaxTokens,
			ToolExchange: cfg.ContextToolExchangePolicy,
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
	manifest := view.Manifest
	manifest.CachePlan = &plan
	manifest.Mutations = ledger

	cfg.System = payload.System
	cfg.Messages = materializeRenderedQuery(payload, cfg.ContextQueryMaterialized)
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
		const anchor = "\n## Workspace instruction files"
		if idx := strings.Index(cfg.System, anchor); idx >= 0 {
			cfg.System = strings.TrimSpace(cfg.System[:idx]) + "\n\n" + usage + "\n" + cfg.System[idx:]
		} else {
			cfg.System = strings.TrimSpace(cfg.System + "\n\n" + usage)
		}
	}
	if text := strings.TrimSpace(cfg.ContextMemoryText); text != "" {
		cfg.Messages = append(cfg.Messages, sdk.UserMessage(text))
		cfg.ContextMemoryText = ""
	}
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
		}
	}
	cfg.ContextQueryMaterialized = true
	return cfg
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
