package runreq

import (
	"context"
	"errors"
	"strings"

	"github.com/felinics/memoh/internal/agent/agentapi/ports"
)

type singleTeamScopes struct{ teamID string }

// SingleTeam is the scope provider of a deployment with one team: it names
// only that team and never enumerates all session rows.
func SingleTeam(teamID string) ports.TeamScopes { return singleTeamScopes{teamID: teamID} }

func (p singleTeamScopes) CurrentScope(context.Context) string { return p.teamID }

func (p singleTeamScopes) ListScopePage(context.Context, string, int32) (ports.TeamScopePage, error) {
	return ports.TeamScopePage{Scopes: []string{p.teamID}, Complete: true}, nil
}

func (singleTeamScopes) BindScope(ctx context.Context, _ string) context.Context {
	return ctx
}

// ErrTeamScopeMismatch reports a scope the provider did not bind to.
var ErrTeamScopeMismatch = errors.New("session resume tenant scope mismatch")

// BindTeamScope returns ctx bound to scope, or ErrTeamScopeMismatch when the
// provider does not report the same scope back.
func BindTeamScope(ctx context.Context, provider ports.TeamScopes, scope string) (context.Context, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return nil, ErrTeamScopeMismatch
	}
	scoped := provider.BindScope(ctx, scope)
	if scoped == nil || strings.TrimSpace(provider.CurrentScope(scoped)) != scope {
		return nil, ErrTeamScopeMismatch
	}
	return scoped, nil
}

// ValidateTeamScopePage rejects a page that is oversized, empty-named, or
// makes no progress past the cursor it was asked for.
func ValidateTeamScopePage(page ports.TeamScopePage, after string) error {
	if len(page.Scopes) > 16 || (!page.Complete && (page.NextCursor == "" || page.NextCursor == after)) {
		return errors.New("invalid session resume scope page")
	}
	for _, scope := range page.Scopes {
		if strings.TrimSpace(scope) == "" {
			return ErrTeamScopeMismatch
		}
	}
	return nil
}
