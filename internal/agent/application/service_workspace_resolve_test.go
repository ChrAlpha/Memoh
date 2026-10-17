package application

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

func TestRuntimeControlContextPinsWorkdirTargetWithoutPrincipal(t *testing.T) {
	perm := &agenttest.CountingPermission{}
	service := workdirBoundService(agenttest.RemoteWorkdir(), agenttest.KindedTargetResolver{}, perm)

	ctx, err := service.runtimeControlContext(t.Context(), RuntimeControlRequest{BotID: "bot-1", ThreadID: "s1"})
	if err != nil {
		t.Fatalf("runtimeControlContext: %v", err)
	}
	if got := workspace.WorkspaceTargetFromContext(ctx); got != "computer-b" || perm.Calls != 0 {
		t.Fatalf("context target = %q, permission calls = %d", got, perm.Calls)
	}
}

// The native schedule entry resolves the workspace itself, ahead of resolve,
// so a bound target that cannot be reached fails the fire instead of running
// in the wrong place.
func TestNativeSchedulePlanBindsWorkspaceBeforeResolve(t *testing.T) {
	boom := errors.New("target unreachable")
	service := workdirBoundService(agenttest.RemoteWorkdir(), agenttest.KindedTargetResolver{Err: boom}, nil)

	_, err := service.nativeSchedulePlan(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "owner-1"}, schedule.TriggerPayload{OwnerUserID: "owner-1"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the workspace resolution failure", err)
	}
}

// fixedWorkdirResolver resolves every binding to the same workdir.
type fixedWorkdirResolver struct{ resolved workdir.Resolved }

func (f fixedWorkdirResolver) ResolveForSession(context.Context, string, string) (workdir.Resolved, error) {
	return f.resolved, nil
}

// workdirBoundService is a Service whose session s1 is bound to the workdir.
func workdirBoundService(resolved workdir.Resolved, targets runconfig.WorkspaceTargetResolver, permissions runreq.BotPermissionChecker) *Service {
	service := wireComponents(&Service{
		sessionService: &agenttest.Sessions{GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{ID: id, BotID: "bot-1", WorkdirID: "p1"}, nil
		}},
		botPermissions: permissions,
	})
	return useRunconfig(service, func(d *runconfig.Deps) {
		d.Workdirs = fixedWorkdirResolver{resolved: resolved}
		d.WorkspaceTargets = targets
	})
}
