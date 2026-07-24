package flow

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
	"github.com/memohai/memoh/internal/models"
)

type continuationParams struct {
	BotID             string
	SessionID         string
	ChannelIdentityID string
	SourcePlatform    string
	ReplyTarget       string
	ConversationType  string
	ChatToken         string
	// SummaryScopeBotID scopes compaction summaries when the originating
	// request carries its own bot id; falls back to BotID when empty.
	SummaryScopeBotID string
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

	base := resolved.RunConfig
	base.ContextBudgetMaxTokens = resolved.ContextBudgetMaxTokens
	cfg, err := r.prepareContinuationRunConfig(
		ctx,
		base,
		historyfrag.ScopeFallback{
			ConversationType: strings.TrimSpace(p.ConversationType),
			ReplyTarget:      strings.TrimSpace(p.ReplyTarget),
		},
		compactionSummaryScope(firstNonEmpty(p.SummaryScopeBotID, p.BotID), "", p.SessionID, p.ConversationType, "", p.ReplyTarget),
		eventCh,
	)
	if err != nil {
		return err
	}

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
