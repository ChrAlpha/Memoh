package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/schedule"
)

// The answers to parked decisions, the resume of interrupted runs and the
// scheduled fires are the answer, resume and schedule components'.

// ContinueCommittedToolApprovalResponse resumes what an approval parked when
// no session runtime routed the answer.
func (s *Service) ContinueCommittedToolApprovalResponse(ctx context.Context, committed decide.CommittedToolApprovalResponse, eventCh chan<- runreq.WSStreamEvent) error {
	return s.answer.ContinueToolApproval(ctx, committed, eventCh)
}

// ContinueCommittedUserInputResponse resumes what a question parked when no
// session runtime routed the answer.
func (s *Service) ContinueCommittedUserInputResponse(ctx context.Context, committed decide.CommittedUserInputResponse, eventCh chan<- runreq.WSStreamEvent) error {
	return s.answer.ContinueUserInput(ctx, committed, eventCh)
}

// StartSessionResume starts the worker that resumes interrupted sessions.
func (s *Service) StartSessionResume(ctx context.Context) error {
	return s.resume.Start(ctx)
}

// StopSessionResume stops discovery only. Active runs are interrupted by the
// session runtime after it persists their marker.
func (s *Service) StopSessionResume() {
	s.resume.Stop()
}

// TriggerSchedule executes a scheduled command via the internal agent.
func (s *Service) TriggerSchedule(ctx context.Context, botID string, payload schedule.TriggerPayload, token string) (schedule.TriggerResult, error) {
	return s.schedule.Trigger(ctx, botID, payload, token)
}
