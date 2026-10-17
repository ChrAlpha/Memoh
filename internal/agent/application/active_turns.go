package application

import (
	"context"
)

// DrainActiveTurns runs after transports cancel their requests and before the
// PostgreSQL pool closes. It is bounded by the caller's shutdown context.
func (s *Service) DrainActiveTurns(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.activeTurns.Drain(ctx)
}
