package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/workdir"
)

// The native schedule entry resolves the workspace itself, ahead of resolve,
// so a bound target that cannot be reached fails the fire instead of running
// in the wrong place.
func TestNativeScheduleSetupBindsWorkspaceBeforeResolve(t *testing.T) {
	boom := errors.New("target unreachable")
	service := workdirBoundService(t, agenttest.RemoteWorkdir(), agenttest.KindedTargetResolver{Err: boom}, nil)

	setup := service.plan.ScheduleSetup(drive.RunSpec{}, schedule.TriggerPayload{OwnerUserID: "owner-1"}, "bot-1", "", externalrun.Dispatch{Kind: externalrun.DispatchNative})
	_, err := setup(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "owner-1"})
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
func workdirBoundService(t *testing.T, resolved workdir.Resolved, targets runconfig.WorkspaceTargetResolver, permissions runreq.BotPermissionChecker) *Service {
	t.Helper()
	return newTestService(t, Deps{
		Sessions: &agenttest.Sessions{GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{ID: id, BotID: "bot-1", WorkdirID: "p1"}, nil
		}},
		BotPermissions:   permissions,
		WorkspaceTargets: targets,
		Workdirs:         fixedWorkdirResolver{resolved: resolved},
	})
}
