package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
)

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
