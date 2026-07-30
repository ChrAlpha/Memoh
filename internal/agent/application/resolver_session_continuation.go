package application

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	historyfrag "github.com/memohai/memoh/internal/agent/context/history"
	"github.com/memohai/memoh/internal/agent/runtime/native"
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
	terminal := s.contextLifecycleTerminal(ctx, cfg)
	var lifecycleCause error
	var lifecycleDeferred bool
	defer func() {
		if !lifecycleDeferred {
			terminal(lifecycleCause)
		}
	}()

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
		if eventErr := agentStreamEventError(event); eventErr != nil && lifecycleCause == nil {
			lifecycleCause = eventErr
		}
		if event.Type == native.EventAgentAbort {
			lifecycleDeferred = strings.TrimSpace(event.ApprovalID) != ""
			if !lifecycleDeferred && lifecycleCause == nil {
				lifecycleCause = errors.New("agent run aborted")
			}
		}
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		if !stored && event.IsTerminal() && len(event.Messages) > 0 {
			if snap, ok := extractTerminalSnapshot(data); ok {
				lifecycleDeferred = lifecycleDeferred || snap.deferredToolID != ""
				if snap.aborted && !lifecycleDeferred && lifecycleCause == nil {
					lifecycleCause = errors.New("agent run aborted")
				}
				if storeErr := s.persistTerminalSnapshot(
					context.WithoutCancel(ctx),
					chatReq,
					resolvedContext{runConfig: cfg, model: models.GetResponse{ID: resolved.ModelID}},
					snap,
				); storeErr != nil {
					lifecycleCause = storeErr
					lifecycleDeferred = false
					return storeErr
				}
				stored = true
			}
		}
		if eventCh != nil {
			select {
			case eventCh <- json.RawMessage(data):
			case <-ctx.Done():
				if lifecycleCause == nil {
					lifecycleCause = context.Cause(ctx)
				}
				return ctx.Err()
			}
		}
	}
	return nil
}
