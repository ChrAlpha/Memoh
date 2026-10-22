package plan

import (
	"context"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// assembleContinuation is the context a decision continuation resumes from:
// the session's history up to and including the stored result of the answered
// call, and no query of its own.
func (p *Planner) assembleContinuation(
	ctx context.Context,
	base native.RunConfig,
	summaryScope contextfrag.Scope,
) (native.RunConfig, error) {
	assembled, err := p.transcript.Assemble(ctx, transcript.Request{
		Purpose: transcript.PurposeContinuation,
		Scope:   summaryScope,
	})
	if err != nil {
		return native.RunConfig{}, err
	}
	messages := runreq.SanitizeMessages(assembled.Messages)
	retained := assembled.Records
	historyEstimates := make([]int, len(messages))
	for i := range messages {
		historyEstimates[i] = transcript.EstimateMessageTokens(messages[i])
	}
	base.ContextHistoryTokenEstimates = historyEstimates
	base.ContextTrimmableMessages = len(messages)

	base.ContextFrags = transcript.ContextFragsForMessages(messages, retained)
	// Close any tool call left open by an interrupted turn before the transcript
	// reaches providers that enforce strict assistant-tool adjacency. A process
	// restart can orphan a deferred ask_user / tool-approval call while a later
	// request still completes normally; repairing here (not in ContextFrags)
	// keeps the fragments faithful to history while the outgoing messages stay
	// provider-valid. Applies to every continuation path that resumes after a
	// deferred tool call.
	base.Messages = runreq.ModelMessagesToSDKMessages(runreq.RepairToolCallClosures(runreq.NonNilModelMessages(messages), runreq.SyntheticToolClosureError))
	base.ContextCurrentUserMessageIndex = nil
	base.ContextMemoryMessageIndex = nil
	base.Query = ""
	return base, nil
}
