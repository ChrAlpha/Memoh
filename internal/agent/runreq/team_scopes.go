package runreq

import (
	"context"
	"errors"
	"strings"
)

// TeamScopePage contains opaque team IDs, never cross-team rows. Hosted
// deployments can reuse their runtime reaper's scope catalog.
type TeamScopePage struct {
	Scopes     []string
	NextCursor string
	Complete   bool
}

// TeamScopes answers which teams this process serves and binds each database
// operation to one of them. Session resume and the discuss sweep both
// enumerate through it. Implementations must enumerate through a restricted
// catalog, then use ordinary team-bound credentials for run queries,
// authorization and execution.
type TeamScopes interface {
	CurrentScope(context.Context) string
	ListScopePage(context.Context, string, int32) (TeamScopePage, error)
	BindScope(context.Context, string) context.Context
}

type singleTeamScopes struct{ teamID string }

// SingleTeam is the scope provider of a deployment with one team: it names
// only that team and never enumerates all session rows.
func SingleTeam(teamID string) TeamScopes { return singleTeamScopes{teamID: teamID} }

func (p singleTeamScopes) CurrentScope(context.Context) string { return p.teamID }

func (p singleTeamScopes) ListScopePage(context.Context, string, int32) (TeamScopePage, error) {
	return TeamScopePage{Scopes: []string{p.teamID}, Complete: true}, nil
}

func (singleTeamScopes) BindScope(ctx context.Context, _ string) context.Context {
	return ctx
}

// ErrTeamScopeMismatch reports a scope the provider did not bind to.
var ErrTeamScopeMismatch = errors.New("session resume tenant scope mismatch")

// BindTeamScope returns ctx bound to scope, or ErrTeamScopeMismatch when the
// provider does not report the same scope back.
func BindTeamScope(ctx context.Context, provider TeamScopes, scope string) (context.Context, error) {
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
func ValidateTeamScopePage(page TeamScopePage, after string) error {
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
