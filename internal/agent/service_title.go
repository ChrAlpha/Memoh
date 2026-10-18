package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/title"
)

// StarterPrompts returns the new-chat suggestions for one user of one bot.
func (s *Service) StarterPrompts(ctx context.Context, req title.StarterPromptsRequest) (title.StarterPromptsResult, error) {
	return s.title.StarterPrompts(ctx, req)
}
