package application

import (
	"context"
	"encoding/json"
	"strings"

	historyfrag "github.com/memohai/memoh/internal/agent/context/history"
	"github.com/memohai/memoh/internal/models"
)

type continuationParams struct {
	RunID             string
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
func (s *Service) resumeAgentSession(ctx context.Context, p continuationParams, eventCh chan<- WSStreamEvent) error {
	resolved, err := s.ResolveRunConfig(ctx, p.BotID, p.SessionID, p.ChannelIdentityID, p.SourcePlatform, p.ReplyTarget, p.ConversationType, p.ChatToken)
	if err != nil {
		return err
	}

	base := resolved.RunConfig
	base.RunID = runIDForChatRequest(p.RunID)
	base.ContextBudgetMaxTokens = resolved.ContextBudgetMaxTokens
	cfg, err := s.prepareContinuationRunConfig(
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

	chatReq := ChatRequest{
		RunID:                   cfg.RunID,
		BotID:                   p.BotID,
		ChatID:                  p.BotID,
		ThreadID:                p.SessionID,
		SourceChannelIdentityID: p.ChannelIdentityID,
		CurrentChannel:          p.SourcePlatform,
		ReplyTarget:             p.ReplyTarget,
		ConversationType:        p.ConversationType,
		UserMessagePersisted:    true,
		WorkspaceTarget:         workspaceTargetFromRunConfig(resolved.RunConfig),
	}

	stream := s.agent.Stream(ctx, cfg)
	stored := false
	for event := range stream {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		if !stored && event.IsTerminal() && len(event.Messages) > 0 {
			if snap, ok := extractTerminalSnapshot(data); ok {
				if storeErr := s.persistTerminalSnapshot(
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
