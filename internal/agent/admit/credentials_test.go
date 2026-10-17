package admit

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/felinics/memoh/internal/auth"
)

func TestSignerRejectsACredentialSignedWithAnotherKey(t *testing.T) {
	t.Parallel()

	signer := NewSigner("resume-secret")
	token, _, err := auth.GenerateToken("account", "other-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Claims(token); err == nil {
		t.Fatal("accepted invalid signature")
	}
}

func TestSignerRenewKeepsTheScopeAndSignsWithItsKey(t *testing.T) {
	t.Parallel()

	signer := NewSigner("resume-secret")
	token, err := signer.Renew(map[string]string{"typ": "chat_route", "bot_id": "bot", "chat_id": "chat", "route_id": "route", "user_id": "sender"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(strings.TrimPrefix(token, "Bearer "), func(_ *jwt.Token) (any, error) { return []byte("resume-secret"), nil }, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatal("renewed token invalid")
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["route_id"] != "route" || claims["typ"] != "chat_route" {
		t.Fatal("scope widened")
	}
	if _, ok := claims["exp"]; !ok {
		t.Fatal("renewed token has no expiry")
	}
}

func TestSignerClaimsKeepsOnlyTheAllowlistedScope(t *testing.T) {
	t.Parallel()

	signer := NewSigner("resume-secret")
	bearer, err := signer.Renew(map[string]string{"typ": "chat_route", "bot_id": "bot", "chat_id": "chat", "secret_field": "must not survive"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.Claims(bearer)
	if err != nil {
		t.Fatal(err)
	}
	if claims["bot_id"] != "bot" || claims["chat_id"] != "chat" {
		t.Fatalf("claims = %v, want the scope", claims)
	}
	if _, ok := claims["secret_field"]; ok {
		t.Fatal("claims kept a field outside the allowlist")
	}
	if got, err := signer.Claims(""); err != nil || got != nil {
		t.Fatalf("Claims(empty) = %v, %v, want nothing", got, err)
	}
}
