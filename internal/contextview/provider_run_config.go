package contextview

import (
	"context"
	"log/slog"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func providerContextViewBuilder() *Builder {
	return NewBuilder(
		NewMapCollectorRegistry(
			&SystemPromptCollector{},
			&HistoryMessagesCollector{},
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
				Messages:        cfg.Messages,
				TokenEstimates:  cfg.ContextHistoryTokenEstimates,
				TrimmablePrefix: cfg.ContextTrimmableMessages,
			}},
			{Name: "current_user", Config: CurrentUserConfig{Query: query}},
			{Name: "inline_images", Config: InlineImageConfig{Images: inlineImages}},
		},
		Targets:         []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
		Budget:          BudgetEnvelope{MaxTokens: cfg.ContextBudgetMaxTokens},
		DynamicMutators: cfg.ContextDynamicMutators,
	})
	if err != nil {
		warnProviderContextView(logger, cfg, "context view build failed; using legacy assembly", err)
		return cfg.RefreshContextFrag()
	}
	payload, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		warnProviderContextView(logger, cfg, "context view rendered unexpected payload; using legacy assembly", nil)
		return cfg.RefreshContextFrag()
	}

	plan := cachePlanFromPlacement(view.Placement)
	ledger := contextfrag.NewMutationLedger()
	manifest := view.Manifest
	manifest.CachePlan = &plan
	manifest.Mutations = ledger

	cfg.System = payload.System
	cfg.Messages = payload.Messages
	cfg.ContextFrags = view.Selected
	cfg.ContextManifest = manifest
	cfg.ContextCachePlan = plan
	cfg.ContextMutations = ledger
	if cfg.ContextLifecycle != nil {
		cfg.ContextLifecycle.SetManifest(manifest)
	}
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
	logger.Warn(msg, attrs...)
}
