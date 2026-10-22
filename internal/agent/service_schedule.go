package agent

import (
	"context"

	"github.com/felinics/memoh/internal/schedule"
)

// TriggerSchedule executes a scheduled command via the internal agent.
func (s *Service) TriggerSchedule(ctx context.Context, botID string, payload schedule.TriggerPayload, token string) (schedule.TriggerResult, error) {
	return s.schedule.Trigger(ctx, botID, payload, token)
}
