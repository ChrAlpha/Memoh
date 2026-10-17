package runconfig

import (
	"context"
	"errors"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
)

// ResolveRunConfig builds a complete RunConfig (model, system prompt, tools,
// identity) for a bot+session without loading messages or requiring a query.
// The caller is responsible for filling RunConfig.Messages.
// Used by discuss turns to reuse the service's model, tools, and prompt pipeline.
func (b *Builder) ForSession(ctx context.Context, botID, sessionID, channelIdentityID, currentPlatform, replyTarget, conversationType, chatToken string) (SessionRunConfig, error) {
	if strings.TrimSpace(botID) == "" {
		return SessionRunConfig{}, errors.New("bot id is required")
	}

	sessionType, runtimeType, stored := b.sessionDescriptor(ctx, sessionID)
	facts := runFacts(stored, sessionType)
	if runtimeType == sessionpkg.RuntimeACPAgent || sessionpkg.IsDirectRuntimeType(runtimeType) {
		// Agent runtimes (ACP and the direct external agents) resolve their
		// own model and prompt; only the identity travels in the run config.
		cfg := native.RunConfig{
			SessionType:   sessionType,
			ReplyDelivery: facts.ReplyDelivery,
			DecisionMode:  facts.DecisionMode,
			Identity: native.SessionContext{
				BotID:             botID,
				ChatID:            botID,
				SessionID:         sessionID,
				ChannelIdentityID: strings.TrimSpace(channelIdentityID),
				CurrentPlatform:   currentPlatform,
				ReplyTarget:       strings.TrimSpace(replyTarget),
				ConversationType:  strings.TrimSpace(conversationType),
				SessionToken:      chatToken,
			},
		}
		return SessionRunConfig{
			RunConfig:   cfg,
			RuntimeType: runtimeType,
		}, nil
	}
	// Resumed turns (ask_user answers, tool approvals, discuss) carry no
	// request model — the session's remembered pair is their model source
	// (issue #879), with the same schedule exclusion as resolve().
	sessionPrefModelID, sessionPrefEffort := "", ""
	if !strings.EqualFold(strings.TrimSpace(sessionType), sessionmode.Schedule) {
		sessionPrefModelID, sessionPrefEffort = b.sessionModelPreference(ctx, sessionID)
	}
	cfg, chatModel, _, err := b.buildBase(ctx, baseParams{
		BotID:              botID,
		SessionID:          sessionID,
		ChannelIdentityID:  channelIdentityID,
		CurrentPlatform:    currentPlatform,
		ReplyTarget:        replyTarget,
		ConversationType:   conversationType,
		SessionToken:       chatToken,
		SessionType:        sessionType,
		Facts:              facts,
		SessionPrefModelID: sessionPrefModelID,
		SessionPrefEffort:  sessionPrefEffort,
	})
	if err != nil {
		return SessionRunConfig{}, err
	}

	contextBudget := b.effectiveContextTokenBudget(chatModel)
	cfg.ContextBudgetMaxTokens = contextBudget
	cfg = b.Prepare(ctx, cfg)
	return SessionRunConfig{
		RunConfig:              cfg,
		ModelID:                chatModel.ID,
		RuntimeType:            runtimeType,
		ContextBudgetMaxTokens: contextBudget,
	}, nil
}
