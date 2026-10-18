package resume

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/runreq"
)

type fixedAccounts struct{ err error }

func (a fixedAccounts) ValidateSession(context.Context, string) error { return a.err }

type fixedPermission struct {
	allowed bool
	err     error
}

func (p fixedPermission) HasBotPermission(context.Context, string, string, string) (bool, error) {
	return p.allowed, p.err
}

func newRenewalWorker(accounts Accounts, permission runreq.BotPermissionChecker) *Worker {
	return New(Deps{
		Logger: slog.New(slog.DiscardHandler), Accounts: accounts, BotPermissions: permission,
		Signer: admit.NewSigner("resume-secret"),
	})
}

func TestRenewalChecksScopeAndAuthorization(t *testing.T) {
	w := newRenewalWorker(fixedAccounts{err: errors.New("session not valid")}, fixedPermission{allowed: true})
	if _, err := w.renewResumeCredential(t.Context(), "bot", map[string]string{"typ": "chat_route", "bot_id": "other", "chat_id": "chat"}); !errors.Is(err, runreq.ErrResumeUnrecoverable) {
		t.Fatalf("cross-bot credential = %v, want an unrecoverable intent", err)
	}
	if _, err := w.renewResumeCredential(t.Context(), "bot", map[string]string{"sub": "account"}); err == nil {
		t.Fatal("renewed account without reauthorization")
	}
	token, err := w.renewResumeCredential(t.Context(), "bot", map[string]string{"typ": "chat_route", "bot_id": "bot", "chat_id": "chat", "route_id": "route", "user_id": "sender"})
	if err != nil || token == "" {
		t.Fatalf("renewed chat-route credential = %q, %v", token, err)
	}
	if got, err := w.renewResumeCredential(t.Context(), "bot", nil); got != "" || err != nil {
		t.Fatalf("no claims = %q, %v, want nothing to renew", got, err)
	}
}

// An account credential is renewed only for an account that is still active and
// still may chat with the bot; one that lost either is an intent that can never
// continue, not a transient failure.
func TestRenewalOfAnAccountCredentialRechecksTheAccountAndItsPermission(t *testing.T) {
	claims := map[string]string{"user_id": "account"}

	token, err := newRenewalWorker(fixedAccounts{}, fixedPermission{allowed: true}).renewResumeCredential(t.Context(), "bot", claims)
	if err != nil || token == "" {
		t.Fatalf("renewed credential of an allowed account = %q, %v", token, err)
	}
	if _, err := newRenewalWorker(fixedAccounts{err: accounts.ErrInactiveAccount}, fixedPermission{allowed: true}).renewResumeCredential(t.Context(), "bot", claims); !errors.Is(err, runreq.ErrResumeUnrecoverable) {
		t.Fatalf("inactive account = %v, want an unrecoverable intent", err)
	}
	if _, err := newRenewalWorker(fixedAccounts{}, fixedPermission{allowed: false}).renewResumeCredential(t.Context(), "bot", claims); !errors.Is(err, runreq.ErrResumeUnrecoverable) {
		t.Fatalf("revoked permission = %v, want an unrecoverable intent", err)
	}
}
