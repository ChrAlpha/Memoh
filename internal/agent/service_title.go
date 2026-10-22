package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
)

// StarterPrompts returns the new-chat suggestions for one user of one bot.
func (s *Service) StarterPrompts(ctx context.Context, req agentapi.StarterPromptsRequest) (agentapi.StarterPromptsResult, error) {
	return s.title.StarterPrompts(ctx, req)
}
