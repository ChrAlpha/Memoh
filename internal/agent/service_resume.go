package agent

import (
	"context"
)

// StartSessionResume starts the worker that resumes interrupted sessions.
func (s *Service) StartSessionResume(ctx context.Context) error {
	return s.resume.Start(ctx)
}

// StopSessionResume stops discovery only. Active runs are interrupted by the
// session runtime after it persists their marker.
func (s *Service) StopSessionResume() {
	s.resume.Stop()
}
