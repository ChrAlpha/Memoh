package flow

import (
	"strings"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
)

func applyRunConfigContextAudit(cfg agentpkg.RunConfig, req conversation.ChatRequest, run TurnRun, displayName string, currentQueryMaterialized bool) agentpkg.RunConfig {
	cfg.ContextScope = contextfrag.Scope{
		BotID:                     strings.TrimSpace(req.BotID),
		ChatID:                    strings.TrimSpace(req.ChatID),
		SessionID:                 strings.TrimSpace(req.SessionID),
		TurnID:                    firstNonEmptyString(run.PersistTurnID, run.Context.TurnID),
		ViewHeadTurnID:            strings.TrimSpace(run.ViewHeadTurnID()),
		ChannelIdentityID:         strings.TrimSpace(req.SourceChannelIdentityID),
		DisplayName:               strings.TrimSpace(firstNonEmptyString(displayName, req.DisplayName)),
		Platform:                  strings.TrimSpace(req.CurrentChannel),
		ConversationType:          strings.TrimSpace(req.ConversationType),
		ConversationName:          strings.TrimSpace(req.ConversationName),
		ReplyTarget:               strings.TrimSpace(req.ReplyTarget),
		CurrentMessageID:          strings.TrimSpace(req.ExternalMessageID),
		EventID:                   strings.TrimSpace(req.EventID),
		ReplyToMessageID:          strings.TrimSpace(req.SourceReplyToMessageID),
		ReplySender:               strings.TrimSpace(req.ReplySender),
		ForwardMessageID:          strings.TrimSpace(req.ForwardMessageID),
		ForwardFromUserID:         strings.TrimSpace(req.ForwardFromUserID),
		ForwardFromConversationID: strings.TrimSpace(req.ForwardFromConversationID),
	}
	cfg.ContextQueryMaterialized = currentQueryMaterialized
	return cfg.RefreshContextFrag()
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
