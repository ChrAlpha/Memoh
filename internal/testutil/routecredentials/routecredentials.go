// Package routecredentials states, once, which identities a channel turn
// carries. The inbound path and the discuss admission each gather the inputs
// of auth.IssueRouteCredentials from their own reads; their tests both hold
// what they sign to the same scenario here, so the two cannot drift apart
// without one of them failing.
package routecredentials

import (
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// Scenario is one participant answered on one route, as the database holds it.
type Scenario struct {
	BotID             string
	RouteID           string
	ChannelIdentityID string
	// ParticipantUserID is the one account the participant's channel identity
	// is bound to; empty when it is bound to none or to several.
	ParticipantUserID string
	// BotOwnerUserID is the account a model session acts for.
	BotOwnerUserID string
	// RuntimeOwnerUserID is the account an External Agent session acts for.
	RuntimeOwnerUserID string
	ExternalRuntime    bool
}

// Want is the identity claims of the chat token and of the session bearer a
// turn answering the scenario carries.
func (s Scenario) Want() (chat, session map[string]string) {
	chatUser := s.ParticipantUserID
	if chatUser == "" {
		chatUser = s.ChannelIdentityID
	}
	chat = map[string]string{
		"typ":                 "chat_route",
		"bot_id":              s.BotID,
		"chat_id":             s.BotID,
		"route_id":            s.RouteID,
		"channel_identity_id": s.ChannelIdentityID,
		"user_id":             chatUser,
	}
	actor := s.BotOwnerUserID
	if s.ExternalRuntime {
		actor = s.RuntimeOwnerUserID
	}
	if actor == "" {
		actor = s.ParticipantUserID
	}
	if actor == "" {
		return chat, chat
	}
	return chat, map[string]string{"sub": actor, "user_id": actor}
}

// Claims reads the identity claims a token carries, ignoring when it was
// signed or expires.
func Claims(t *testing.T, secret, bearer string) map[string]string {
	t.Helper()
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bearer), "Bearer "))
	if raw == "" {
		t.Fatal("token is empty")
	}
	token, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return []byte(secret), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		t.Fatalf("parse token: %v", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims are %T", token.Claims)
	}
	out := map[string]string{}
	for _, key := range []string{"sub", "user_id", "typ", "bot_id", "chat_id", "route_id", "channel_identity_id"} {
		if value, ok := claims[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	return out
}
