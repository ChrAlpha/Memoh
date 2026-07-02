package flow

import (
	"context"
	"log/slog"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
)

func providerContextViewBuilder() *contextview.Builder {
	return contextview.NewBuilder(
		contextview.NewMapCollectorRegistry(
			&contextview.SystemPromptCollector{},
			&contextview.HistoryMessagesCollector{},
			&contextview.CurrentUserCollector{},
			&contextview.InlineImageCollector{},
		),
		contextview.PassthroughSelector{},
		contextview.IdentityPlacer{},
		contextview.NewMapRendererRegistry(&contextview.SDKMessagesRenderer{}),
	)
}

func applyProviderContextView(ctx context.Context, logger *slog.Logger, cfg agentpkg.RunConfig) agentpkg.RunConfig {
	query := cfg.Query
	if cfg.ContextQueryMaterialized {
		query = ""
	}
	view, err := providerContextViewBuilder().Build(ctx, contextview.BuildInput{
		Scope:  cfg.ContextScope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Sources: []contextview.SourceSpec{
			{Name: "system_prompt", Config: contextview.SystemPromptConfig{System: cfg.System, ToolUsage: cfg.ContextToolUsage}},
			{Name: "history_messages", Config: contextview.HistoryMessagesConfig{Messages: cfg.Messages}},
			{Name: "current_user", Config: contextview.CurrentUserConfig{Query: query}},
			{Name: "inline_images", Config: contextview.InlineImageConfig{Images: cfg.InlineImages}},
		},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
	})
	if err != nil {
		warnProviderContextView(logger, cfg, "context view build failed; using legacy assembly", err)
		return cfg.RefreshContextFrag()
	}
	payload, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*contextview.SDKRenderedPayload)
	if !ok {
		warnProviderContextView(logger, cfg, "context view rendered unexpected payload; using legacy assembly", nil)
		return cfg.RefreshContextFrag()
	}

	cfg.System = payload.System
	cfg.Messages = payload.Messages
	cfg.ContextFrags = view.Selected
	cfg.ContextManifest = view.Manifest
	return cfg
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
