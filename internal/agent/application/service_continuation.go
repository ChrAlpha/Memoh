package application

import (
	"context"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/transcript"
)

func (s *Service) prepareContinuationRunConfig(
	ctx context.Context,
	base native.RunConfig,
	fallback historyfrag.ScopeFallback,
	summaryScope contextfrag.Scope,
	live bool,
) (native.RunConfig, error) {
	assembled, err := s.transcript.Assemble(ctx, transcript.Request{
		Purpose:  transcript.PurposeContinuation,
		Fallback: fallback,
		Scope:    summaryScope,
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
	// A question asked with nobody receiving the run's events could never be
	// answered, so asking one is only offered to a run someone follows live.
	base.LiveToolStream = live
	base.CanRequestUserInput = live
	return s.runconfig.Prepare(ctx, base), nil
}
