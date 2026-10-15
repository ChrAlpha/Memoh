package auth

import (
	"strings"
	"time"
)

// RouteCredentials names who one channel turn answers and who it acts for.
// The inbound path, the discuss admission and the queue all issue a turn's
// credentials through IssueRouteCredentials, so a turn started by any of them
// carries the same identities. It holds identities and no secret, which is
// what lets a queued turn store it and sign again when it starts.
type RouteCredentials struct {
	BotID   string `json:"bot_id"`
	ChatID  string `json:"chat_id"`
	RouteID string `json:"route_id,omitempty"`
	// ReplyTarget is the conversation the turn answers in. A turn without one
	// has nowhere to reply, so it gets no chat token.
	ReplyTarget string `json:"reply_target,omitempty"`
	// ChannelIdentityID is the external participant the turn answers.
	ChannelIdentityID string `json:"channel_identity_id,omitempty"`
	// UserID is the account bound to ChannelIdentityID; empty when unbound.
	UserID string `json:"user_id,omitempty"`
	// ExternalRuntime is set for a session an External Agent runtime drives.
	// Such a session acts for its runtime owner; any other acts for the bot's
	// owner.
	ExternalRuntime    bool   `json:"external_runtime,omitempty"`
	RuntimeOwnerUserID string `json:"runtime_owner_user_id,omitempty"`
	BotOwnerUserID     string `json:"bot_owner_user_id,omitempty"`
}

// IssueRouteCredentials signs a turn's chat token and its session bearer.
//
// The chat token routes replies back to the conversation and names the
// participant. The session bearer is a user token for the account the session
// acts for, falling back to the participant's own account, and then to the
// chat token itself. Without a secret neither is issued.
func IssueRouteCredentials(c RouteCredentials, secret string, ttl time.Duration) (chatToken, sessionBearer string, err error) {
	if strings.TrimSpace(secret) == "" {
		return "", "", nil
	}
	if strings.TrimSpace(c.ReplyTarget) != "" {
		chatToken, _, err = GenerateChatToken(ChatToken{
			BotID:             c.BotID,
			ChatID:            c.ChatID,
			RouteID:           strings.TrimSpace(c.RouteID),
			UserID:            c.UserID,
			ChannelIdentityID: c.ChannelIdentityID,
		}, secret, ttl)
		if err != nil {
			return "", "", err
		}
	}
	actor := strings.TrimSpace(c.BotOwnerUserID)
	if c.ExternalRuntime {
		actor = strings.TrimSpace(c.RuntimeOwnerUserID)
	}
	if actor == "" {
		actor = strings.TrimSpace(c.UserID)
	}
	if actor == "" {
		if chatToken == "" {
			return "", "", nil
		}
		return chatToken, "Bearer " + chatToken, nil
	}
	signed, _, err := GenerateToken(actor, secret, ttl)
	if err != nil {
		return "", "", err
	}
	return chatToken, "Bearer " + signed, nil
}
