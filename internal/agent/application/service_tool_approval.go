package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/runreq"
)

func (s *Service) respondToolApproval(ctx context.Context, input decide.ToolApprovalResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := s.decider.CommitToolApprovalResponse(ctx, input)
	if err != nil {
		return err
	}
	return s.ContinueCommittedToolApprovalResponse(ctx, committed, eventCh)
}

// ContinueCommittedToolApprovalResponse resumes what an approval parked when
// no session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (s *Service) ContinueCommittedToolApprovalResponse(ctx context.Context, committed decide.CommittedToolApprovalResponse, eventCh chan<- runreq.WSStreamEvent) error {
	target := committed.Request()
	if committed.AckOnly() {
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	if committed.UsesDecisionWaiter() {
		if committed.ActivePrompt() != nil {
			return forwardExternalAgentActivePrompt(ctx, committed.ActivePrompt(), eventCh, externalAgentActivePromptForwardOptions{
				SkipToolCallID: target.ToolCallID,
				SkipApprovalID: target.ID,
			})
		}
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	return s.driveUnroutedContinuation(ctx, s.toolApprovalContinuation(ctx, committed, eventCh != nil), eventCh)
}

// toolApprovalContinuation is the run an answered approval resumes.
func (s *Service) toolApprovalContinuation(ctx context.Context, committed decide.CommittedToolApprovalResponse, live bool) decisionContinuation {
	return s.decidedContinuation(ctx, s.decider.ToolApprovalCall(committed), live)
}

// CanDecideToolApproval is the single authorization for answering a tool
// approval; see decide.Decider.CanDecideToolApproval.
func (s *Service) CanDecideToolApproval(ctx context.Context, approval toolapproval.Request, principal decide.ToolApprovalPrincipal) (bool, error) {
	return s.decider.CanDecideToolApproval(ctx, approval, principal)
}

// ViewerCanDecideToolApproval answers CanDecideToolApproval for an approval a
// live frame names only by id.
func (s *Service) ViewerCanDecideToolApproval(ctx context.Context, approvalID string, principal decide.ToolApprovalPrincipal) (bool, error) {
	return s.decider.ViewerCanDecideToolApproval(ctx, approvalID, principal)
}
