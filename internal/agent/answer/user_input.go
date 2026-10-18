package answer

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// RespondUserInput commits a user-input answer the runtime did not route and continues what it parked.
func (r *Responder) RespondUserInput(ctx context.Context, input decide.UserInputResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := r.decider.CommitUserInputResponse(ctx, input)
	if err != nil {
		return err
	}
	return r.ContinueUserInput(ctx, committed, eventCh)
}

// ContinueUserInput resumes what a question parked when no
// session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (r *Responder) ContinueUserInput(ctx context.Context, committed decide.CommittedUserInputResponse, eventCh chan<- runreq.WSStreamEvent) error {
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
	return r.DriveUnrouted(ctx, r.UserInputContinuation(ctx, committed, eventCh != nil), eventCh)
}

// UserInputContinuation is the run an answered question resumes.
func (r *Responder) UserInputContinuation(ctx context.Context, committed decide.CommittedUserInputResponse, live bool) Continuation {
	return r.DecidedContinuation(ctx, r.decider.UserInputCall(committed), live)
}
