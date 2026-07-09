package flow

import (
	"context"
	"encoding/json"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/models"
)

// finalizeContinuationRunConfig mirrors resolve()'s post-sanitize ordering
// (sanitize, then snapshot history estimates/trimmable count, then build the
// SDK message list) so tool-approval and user-input session resumption carry
// the same budget-trimming and tool-exchange-policy signals as the main chat
// path.
func finalizeContinuationRunConfig(cfg agentpkg.RunConfig, messages []conversation.ModelMessage, contextBudgetMaxTokens int, liveToolStream, canRequestUserInput bool) agentpkg.RunConfig {
	messages = sanitizeMessages(messages)
	historyEstimates := make([]int, len(messages))
	for i := range messages {
		historyEstimates[i] = estimateMessageTokens(messages[i])
	}
	cfg.ContextHistoryTokenEstimates = historyEstimates
	cfg.ContextTrimmableMessages = len(messages)
	if cfg.ContextToolExchangePolicy == nil {
		cfg.ContextToolExchangePolicy = defaultToolExchangePolicy()
	}
	if cfg.ContextBudgetMaxTokens == 0 {
		cfg.ContextBudgetMaxTokens = contextBudgetMaxTokens
	}
	cfg.Messages = modelMessagesToSDKMessages(nonNilModelMessages(messages))
	cfg.ContextCurrentUserMessageIndex = nil
	cfg.Query = ""
	cfg.LiveToolStream = liveToolStream
	cfg.CanRequestUserInput = canRequestUserInput
	return cfg
}

type continuationParams struct {
	BotID             string
	SessionID         string
	ChannelIdentityID string
	SourcePlatform    string
	ReplyTarget       string
	ConversationType  string
	ChatToken         string
}

// resumeAgentSession is the shared core of continueToolApprovalSession and
// continueUserInputSession: resolve the run config, load+prune+decompact
// history, stream the agent, and persist the terminal snapshot on the first
// terminal event. p.ChannelIdentityID must already be the firstNonEmpty(...)
// resolved value.
func (r *Resolver) resumeAgentSession(ctx context.Context, p continuationParams, eventCh chan<- WSStreamEvent) error {
	resolved, err := r.ResolveRunConfig(ctx, p.BotID, p.SessionID, p.ChannelIdentityID, p.SourcePlatform, p.ReplyTarget, p.ConversationType, p.ChatToken)
	if err != nil {
		return err
	}

	loaded, err := r.loadMessages(ctx, p.BotID, p.SessionID, defaultMaxContextMinutes)
	if err != nil {
		return err
	}
	loaded = pruneHistoryForGateway(loaded)
	loaded = r.replaceCompactedMessages(ctx, loaded)
	messages := modelMessagesOf(loaded)

	cfg := finalizeContinuationRunConfig(resolved.RunConfig, messages, resolved.ContextBudgetMaxTokens, eventCh != nil, r.canDeliverUserInputWS(eventCh))
	cfg = r.prepareRunConfig(ctx, cfg)

	chatReq := conversation.ChatRequest{
		BotID:                   p.BotID,
		ChatID:                  p.BotID,
		SessionID:               p.SessionID,
		SourceChannelIdentityID: p.ChannelIdentityID,
		CurrentChannel:          p.SourcePlatform,
		ReplyTarget:             p.ReplyTarget,
		ConversationType:        p.ConversationType,
		UserMessagePersisted:    true,
	}

	stream := r.agent.Stream(ctx, cfg)
	stored := false
	for event := range stream {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		if !stored && event.IsTerminal() && len(event.Messages) > 0 {
			if snap, ok := extractTerminalSnapshot(data); ok {
				if storeErr := r.persistTerminalSnapshot(
					context.WithoutCancel(ctx),
					chatReq,
					resolvedContext{runConfig: cfg, model: models.GetResponse{ID: resolved.ModelID}},
					snap,
				); storeErr != nil {
					return storeErr
				}
				stored = true
			}
		}
		if eventCh != nil {
			select {
			case eventCh <- json.RawMessage(data):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}
