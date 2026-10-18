package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
)

func (s *Service) respondUserInput(ctx context.Context, input decide.UserInputResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := s.decider.CommitUserInputResponse(ctx, input)
	if err != nil {
		return err
	}
	return s.ContinueCommittedUserInputResponse(ctx, committed, eventCh)
}

// ContinueCommittedUserInputResponse resumes what a question parked when no
// session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (s *Service) ContinueCommittedUserInputResponse(ctx context.Context, committed decide.CommittedUserInputResponse, eventCh chan<- runreq.WSStreamEvent) error {
	resolved := committed.Request()
	if committed.AckOnly() {
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	if committed.IsExternalAgent() {
		// A waiter inside the running turn is blocked on this request and
		// resumes the run itself. When this response stream has reattached to the active ACP
		// prompt, forward that live continuation so refreshes observe the same
		// loading/progress shape as native deferred requests.
		if committed.ActivePrompt() != nil {
			return forwardExternalAgentActivePrompt(ctx, committed.ActivePrompt(), eventCh, externalAgentActivePromptForwardOptions{
				SkipToolCallID:  resolved.ToolCallID,
				SkipUserInputID: resolved.ID,
			})
		}
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	return s.driveUnroutedContinuation(ctx, s.userInputContinuation(ctx, committed, eventCh != nil), eventCh)
}

// userInputContinuation is the run an answered question resumes.
func (s *Service) userInputContinuation(ctx context.Context, committed decide.CommittedUserInputResponse, live bool) decisionContinuation {
	return s.decidedContinuation(ctx, s.decider.UserInputCall(committed), live)
}
