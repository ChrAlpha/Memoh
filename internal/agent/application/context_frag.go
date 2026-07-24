package application

import (
	"strings"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/agent/sessionmode"
)

func buildContextFragScope(req ChatRequest, displayName string, identity native.SessionContext) contextfrag.Scope {
	channelIdentityID := firstNonEmpty(req.SourceChannelIdentityID, identity.ChannelIdentityID)
	scope := contextfrag.Scope{
		BotID:                     firstNonEmpty(req.BotID, identity.BotID),
		ChatID:                    firstNonEmpty(req.ChatID, identity.ChatID),
		SessionID:                 firstNonEmpty(req.ThreadID, identity.SessionID),
		ChannelIdentityID:         strings.TrimSpace(channelIdentityID),
		DisplayName:               strings.TrimSpace(displayName),
		Platform:                  firstNonEmpty(req.CurrentChannel, identity.CurrentPlatform),
		ConversationType:          firstNonEmpty(req.ConversationType, identity.ConversationType),
		ConversationName:          strings.TrimSpace(req.ConversationName),
		ReplyTarget:               firstNonEmpty(req.ReplyTarget, identity.ReplyTarget),
		CurrentMessageID:          strings.TrimSpace(req.ExternalMessageID),
		EventID:                   strings.TrimSpace(req.EventID),
		ReplyToMessageID:          strings.TrimSpace(req.SourceReplyToMessageID),
		ReplySender:               strings.TrimSpace(req.ReplySender),
		MentionsBot:               req.MentionsBot,
		RepliesToBot:              req.RepliesToBot,
		ForwardMessageID:          strings.TrimSpace(req.ForwardMessageID),
		ForwardFromUserID:         strings.TrimSpace(req.ForwardFromUserID),
		ForwardFromConversationID: strings.TrimSpace(req.ForwardFromConversationID),
	}
	scope.Attention = contextFragAttentionReasons(req)
	return scope
}

func contextFragAttentionReasons(req ChatRequest) []contextfrag.AttentionReason {
	derivation := contextfrag.AttentionDerivation{
		ConversationType: req.ConversationType,
		MentionsBot:      req.MentionsBot,
		RepliesToBot:     req.RepliesToBot,
		UnknownTypeKind:  "direct",
	}
	switch strings.TrimSpace(req.SessionType) {
	case sessionmode.Schedule:
		derivation.Leading = []contextfrag.AttentionReason{contextfrag.AttentionSchedule}
	case sessionmode.Heartbeat:
		derivation.Leading = []contextfrag.AttentionReason{contextfrag.AttentionHeartbeat}
	}
	if query := strings.TrimSpace(firstNonEmpty(req.RawQuery, req.Query)); strings.HasPrefix(query, "/") {
		derivation.Trailing = []contextfrag.AttentionReason{contextfrag.AttentionCommand}
	}
	return contextfrag.DeriveAttention(derivation)
}
