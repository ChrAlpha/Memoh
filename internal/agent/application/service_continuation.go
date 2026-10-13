package application

import (
	"context"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

type continuationTurnRequestMessageResolver interface {
	GetVisibleHistoryTurnRequestMessageIDByTurn(context.Context, string, string) (string, error)
}

func (s *Service) continuationTurnRequestMessageID(ctx context.Context, sessionID string, runHandle sessionruntime.RunHandle) (string, error) {
	turnID := strings.TrimSpace(runHandle.TurnID)
	if turnID == "" || s == nil || s.messageService == nil {
		return "", nil
	}
	resolver, ok := s.messageService.(continuationTurnRequestMessageResolver)
	if !ok {
		return "", nil
	}
	return resolver.GetVisibleHistoryTurnRequestMessageIDByTurn(ctx, sessionID, turnID)
}

func (s *Service) prepareContinuationRunConfig(
	ctx context.Context,
	base native.RunConfig,
	fallback historyfrag.ScopeFallback,
	summaryScope contextfrag.Scope,
	live bool,
) (native.RunConfig, error) {
	assembled, err := s.assembleContext(ctx, contextRequest{
		Purpose:  contextPurposeContinuation,
		Fallback: fallback,
		Scope:    summaryScope,
	})
	if err != nil {
		return native.RunConfig{}, err
	}
	messages := sanitizeMessages(assembled.Messages)
	retained := assembled.Records
	historyEstimates := make([]int, len(messages))
	for i := range messages {
		historyEstimates[i] = estimateMessageTokens(messages[i])
	}
	base.ContextHistoryTokenEstimates = historyEstimates
	base.ContextTrimmableMessages = len(messages)

	base.ContextFrags = historyContextFragsForMessages(messages, retained)
	// Close any tool call left open by an interrupted turn before the transcript
	// reaches providers that enforce strict assistant-tool adjacency. A process
	// restart can orphan a deferred ask_user / tool-approval call while a later
	// request still completes normally; repairing here (not in ContextFrags)
	// keeps the fragments faithful to history while the outgoing messages stay
	// provider-valid. Applies to every continuation path that resumes after a
	// deferred tool call.
	base.Messages = modelMessagesToSDKMessages(repairToolCallClosures(nonNilModelMessages(messages), syntheticToolClosureError))
	base.ContextCurrentUserMessageIndex = nil
	base.ContextMemoryMessageIndex = nil
	if base.ContextToolExchangePolicy == nil {
		base.ContextToolExchangePolicy = defaultToolExchangePolicy()
	}
	base.Query = ""
	// A question asked with nobody receiving the run's events could never be
	// answered, so asking one is only offered to a run someone follows live.
	base.LiveToolStream = live
	base.CanRequestUserInput = live && s.userInput != nil
	return s.prepareRunConfig(ctx, base), nil
}
