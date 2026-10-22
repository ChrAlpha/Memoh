package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// continueCommittedToolApprovalResponse resumes what an approval parked when
// no session runtime routed the answer.
func (s *Service) continueCommittedToolApprovalResponse(ctx context.Context, committed decide.CommittedToolApprovalResponse, eventCh chan<- runreq.WSStreamEvent) error {
	return s.answer.ContinueToolApproval(ctx, committed, eventCh)
}

// continueCommittedUserInputResponse resumes what a question parked when no
// session runtime routed the answer.
func (s *Service) continueCommittedUserInputResponse(ctx context.Context, committed decide.CommittedUserInputResponse, eventCh chan<- runreq.WSStreamEvent) error {
	return s.answer.ContinueUserInput(ctx, committed, eventCh)
}
