package answer

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// RespondToolApproval commits an approval answer the runtime did not route and continues what it parked.
func (r *Responder) RespondToolApproval(ctx context.Context, input decide.ToolApprovalResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := r.decider.CommitToolApprovalResponse(ctx, input)
	if err != nil {
		return err
	}
	return r.ContinueToolApproval(ctx, committed, eventCh)
}

// ContinueToolApproval resumes what an approval parked when
// no session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (r *Responder) ContinueToolApproval(ctx context.Context, committed decide.CommittedToolApprovalResponse, eventCh chan<- runreq.WSStreamEvent) error {
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
	return r.DriveUnrouted(ctx, r.ToolApprovalContinuation(ctx, committed, eventCh != nil), eventCh)
}

// ToolApprovalContinuation is the run an answered approval resumes.
func (r *Responder) ToolApprovalContinuation(ctx context.Context, committed decide.CommittedToolApprovalResponse, live bool) Continuation {
	return r.DecidedContinuation(ctx, r.decider.ToolApprovalCall(committed), live)
}
