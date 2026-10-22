package externalrun

import (
	"context"
	"testing"

	"github.com/felinics/memoh/internal/agent/agentapi/ports"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/turn"
	session "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

func TestRuntimeControlContextPinsWorkdirTargetWithoutPrincipal(t *testing.T) {
	perm := &agenttest.CountingPermission{}
	service := workdirBoundHost(t, agenttest.RemoteWorkdir(), agenttest.KindedTargetResolver{}, perm)

	ctx, err := service.runtimeControlContext(t.Context(), turn.RuntimeControlRequest{BotID: "bot-1", ThreadID: "s1"})
	if err != nil {
		t.Fatalf("runtimeControlContext: %v", err)
	}
	if got := workspace.WorkspaceTargetFromContext(ctx); got != "computer-b" || perm.Calls != 0 {
		t.Fatalf("context target = %q, permission calls = %d", got, perm.Calls)
	}
}

// fixedWorkdirResolver resolves every binding to the same workdir.
type fixedWorkdirResolver struct{ resolved workdir.Resolved }

func (f fixedWorkdirResolver) ResolveForSession(context.Context, string, string) (workdir.Resolved, error) {
	return f.resolved, nil
}

// workdirBoundHost is a Host whose session s1 is bound to the workdir.
func workdirBoundHost(t *testing.T, resolved workdir.Resolved, targets ports.WorkspaceTargetResolver, permissions ports.BotPermissionChecker) *Host {
	t.Helper()
	sessions := &agenttest.Sessions{GetFn: func(_ context.Context, id string) (session.Thread, error) {
		return session.Thread{ID: id, BotID: "bot-1", WorkdirID: "p1"}, nil
	}}
	host := withRunconfig(t, func(d *runconfig.Deps) {
		d.Sessions = sessions
		d.BotPermissions = permissions
		d.Workdirs = fixedWorkdirResolver{resolved: resolved}
		d.WorkspaceTargets = targets
	})
	return host
}
