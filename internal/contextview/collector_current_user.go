package contextview

import (
	"context"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

const currentUserCollectorName = "current_user"

type CurrentUserConfig struct {
	Query string
}

type CurrentUserCollector struct{}

func (*CurrentUserCollector) Name() string {
	return currentUserCollectorName
}

func (*CurrentUserCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := currentUserConfig(req.Config)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(cfg.Query)
	if query == "" {
		return nil, nil
	}
	return []contextfrag.ContextFrag{
		contextfrag.TextFrag(contextfrag.TextFragInput{
			ID:         "current_user.message",
			Kind:       contextfrag.KindCurrentUserMessage,
			Role:       sdk.MessageRoleUser,
			Slot:       contextfrag.SlotCurrentUser,
			Text:       query,
			Priority:   90,
			CacheClass: contextfrag.CacheNever,
			Trust:      contextfrag.TrustUser,
			Scope:      req.Scope,
			Source:     contextfrag.SourceRunConfig,
			Collector:  currentUserCollectorName,
			Render:     contextfrag.RenderPolicy{Format: contextfrag.RenderSDKMessage},
		}),
	}, nil
}

func currentUserConfig(config any) (CurrentUserConfig, error) {
	return collectorConfig[CurrentUserConfig](config, "current_user config must be CurrentUserConfig")
}
