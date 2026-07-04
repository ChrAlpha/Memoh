package contextview

import (
	"context"
	"log/slog"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func providerContextViewBuilder() *Builder {
	return NewBuilder(
		NewMapCollectorRegistry(
			&SystemPromptCollector{},
			&HistoryMessagesCollector{},
			&MemoryContextCollector{},
			&CurrentUserCollector{},
			&InlineImageCollector{},
		),
		&FragmentSelector{},
		StablePrefixPlacer{},
		NewMapRendererRegistry(&SDKMessagesRenderer{}),
	)
}

// ProviderRunConfigApplier adapts ApplyProviderRunConfig to the agent's
// injected applier hook.
func ProviderRunConfigApplier(logger *slog.Logger) agentpkg.ContextViewApplier {
	return func(ctx context.Context, cfg agentpkg.RunConfig) agentpkg.RunConfig {
		return ApplyProviderRunConfig(ctx, logger, cfg)
	}
}

// ApplyProviderRunConfig rebuilds the provider-facing run config through the
// context view pipeline: collect from the materialized RunConfig fields,
// select under the token budget, place for prompt caching and render the SDK
// payload back onto the config together with its manifest and cache plan.
func ApplyProviderRunConfig(ctx context.Context, logger *slog.Logger, cfg agentpkg.RunConfig) agentpkg.RunConfig {
	query := cfg.Query
	inlineImages := cfg.InlineImages
	if cfg.ContextQueryMaterialized {
		query = ""
		inlineImages = nil
	}
	view, err := providerContextViewBuilder().Build(ctx, BuildInput{
		Scope:  cfg.ContextScope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Sources: []SourceSpec{
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
		},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
		Budget: BudgetEnvelope{
			MaxTokens:    cfg.ContextBudgetMaxTokens,
			ToolExchange: cfg.ContextToolExchangePolicy,
		},
		DynamicMutators: cfg.ContextDynamicMutators,
	})
	if err != nil {
		warnProviderContextView(logger, cfg, "context view build failed; using legacy assembly", err)
		return legacyMaterializeQuery(cfg).RefreshContextFrag()
	}
	payload, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		warnProviderContextView(logger, cfg, "context view rendered unexpected payload; using legacy assembly", nil)
		return legacyMaterializeQuery(cfg).RefreshContextFrag()
	}

	plan := cachePlanFromPlacement(view.Placement)
	ledger := contextfrag.NewMutationLedger()
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

// legacyMaterializeQuery reproduces the pre-view query placement for the
// build-error fallback, so a broken view degrades to the legacy assembly
// instead of silently dropping the current request and memory recall.
func legacyMaterializeQuery(cfg agentpkg.RunConfig) agentpkg.RunConfig {
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
