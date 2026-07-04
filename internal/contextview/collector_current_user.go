package contextview

import (
	"context"
	"errors"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
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
	if config == nil {
		return CurrentUserConfig{}, nil
	}
	switch cfg := config.(type) {
	case CurrentUserConfig:
		return cfg, nil
	case *CurrentUserConfig:
		if cfg == nil {
			return CurrentUserConfig{}, nil
		}
		return *cfg, nil
	default:
		return CurrentUserConfig{}, errors.New("current_user config must be CurrentUserConfig")
	}
}
