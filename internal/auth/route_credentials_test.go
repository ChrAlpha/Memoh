package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const routeTestSecret = "route-secret"

func routeClaims(t *testing.T, raw string) jwt.MapClaims {
	t.Helper()
	token, err := jwt.Parse(strings.TrimPrefix(raw, "Bearer "), func(*jwt.Token) (any, error) { return []byte(routeTestSecret), nil })
	if err != nil || !token.Valid {
		t.Fatalf("parse %q: %v", raw, err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims of %q are %T", raw, token.Claims)
	}
	return claims
}

func issueRoute(t *testing.T, c RouteCredentials) (string, string) {
	t.Helper()
	chat, bearer, err := IssueRouteCredentials(c, routeTestSecret, 5*time.Minute)
	if err != nil {
		t.Fatalf("IssueRouteCredentials(%+v) error = %v", c, err)
	}
	return chat, bearer
}

var routeTestBase = RouteCredentials{
	BotID: "bot-1", ChatID: "bot-1", RouteID: "route-1", ReplyTarget: "group-1",
	ChannelIdentityID: "identity-1", UserID: "user-sender",
	BotOwnerUserID: "user-owner", RuntimeOwnerUserID: "user-runtime",
}

// The chat token routes the reply and names the participant it answers.
func TestIssueRouteCredentialsChatTokenNamesRouteAndParticipant(t *testing.T) {
	chat, _ := issueRoute(t, routeTestBase)
	claims := routeClaims(t, chat)
	for key, want := range map[string]string{
		claimType: chatTokenType, claimBotID: "bot-1", claimChatID: "bot-1", claimRouteID: "route-1",
		claimChannelIdentityID: "identity-1", claimUserID: "user-sender",
	} {
		if claims[key] != want {
			t.Fatalf("chat token %s = %v, want %q", key, claims[key], want)
		}
	}

	// An unbound participant is named by its channel identity.
	unbound := routeTestBase
	unbound.UserID = ""
	chat, _ = issueRoute(t, unbound)
	if got := routeClaims(t, chat)[claimUserID]; got != "identity-1" {
		t.Fatalf("unbound chat token user_id = %v, want the channel identity", got)
	}
}

// The session bearer acts for the bot owner, or for the runtime owner of an
// External Agent session, and falls back to the participant, then to the chat
// token.
func TestIssueRouteCredentialsSessionBearerActor(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RouteCredentials)
		want   string
	}{
		{"model runtime acts for the bot owner", func(*RouteCredentials) {}, "user-owner"},
		{"external runtime acts for the runtime owner", func(c *RouteCredentials) { c.ExternalRuntime = true }, "user-runtime"},
		{"no owner falls back to the participant", func(c *RouteCredentials) { c.BotOwnerUserID = "" }, "user-sender"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := routeTestBase
			tc.mutate(&c)
			_, bearer := issueRoute(t, c)
			if got := routeClaims(t, bearer)[claimUserID]; got != tc.want {
				t.Fatalf("session bearer acts for %v, want %q", got, tc.want)
			}
		})
	}

	nobody := routeTestBase
	nobody.UserID, nobody.BotOwnerUserID = "", ""
	chat, bearer := issueRoute(t, nobody)
	if bearer != "Bearer "+chat {
		t.Fatalf("session bearer with no account = %q, want the chat token", bearer)
	}
}

// A turn with nowhere to reply gets no chat token, and nothing is issued
// without a secret.
func TestIssueRouteCredentialsWithoutReplyTargetOrSecret(t *testing.T) {
	noTarget := routeTestBase
	noTarget.ReplyTarget = ""
	chat, bearer := issueRoute(t, noTarget)
	if chat != "" {
		t.Fatalf("chat token without reply target = %q, want none", chat)
	}
	if got := routeClaims(t, bearer)[claimUserID]; got != "user-owner" {
		t.Fatalf("session bearer without reply target acts for %v, want the bot owner", got)
	}

	chat, bearer, err := IssueRouteCredentials(routeTestBase, "", 5*time.Minute)
	if err != nil || chat != "" || bearer != "" {
		t.Fatalf("without secret: chat=%q bearer=%q err=%v, want nothing", chat, bearer, err)
	}
}

// An External Agent session acts for its runtime owner alone. Without one no
// credentials are issued: the participant who wrote the input is not the
// session's owner, and acting as them would run the runtime with their
// authority.
func TestIssueRouteCredentialsRefusesAnExternalSessionWithoutItsOwner(t *testing.T) {
	c := routeTestBase
	c.ExternalRuntime = true
	c.RuntimeOwnerUserID = " "
	chat, bearer, err := IssueRouteCredentials(c, routeTestSecret, 5*time.Minute)
	if !errors.Is(err, ErrRuntimeOwnerMissing) || chat != "" || bearer != "" {
		t.Fatalf("IssueRouteCredentials() = %q, %q, %v; want nothing and ErrRuntimeOwnerMissing", chat, bearer, err)
	}
}
