package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/schedule"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

// kindedTargetResolver reports native for the native id and remote otherwise;
// an empty id resolves to the context-pinned target, then to native.
type kindedTargetResolver struct{ err error }

func (r kindedTargetResolver) ResolveWorkspaceTarget(ctx context.Context, _ string, targetID string) (workspace.ResolvedWorkspaceTarget, error) {
	if r.err != nil {
		return workspace.ResolvedWorkspaceTarget{}, r.err
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		targetID = workspace.WorkspaceTargetFromContext(ctx)
	}
	if targetID == "" || targetID == workspace.WorkspaceTargetNative {
		return workspace.ResolvedWorkspaceTarget{TargetID: workspace.WorkspaceTargetNative, Kind: workspace.WorkspaceTargetNative, Name: "Server Workspace"}, nil
	}
	return workspace.ResolvedWorkspaceTarget{TargetID: targetID, Kind: workspace.WorkspaceTargetRemote, Name: "Computer B"}, nil
}

type countingPermission struct {
	allowed bool
	calls   int
}

func (c *countingPermission) HasBotPermission(context.Context, string, string, string) (bool, error) {
	c.calls++
	return c.allowed, nil
}

func remoteWorkdir() workdir.Resolved {
	return workdir.Resolved{WorkdirID: "p1", TargetID: "computer-b", Kind: workdir.TargetKindRemote, WorkDir: "/Users/alice/code"}
}

func nativeWorkdir() workdir.Resolved {
	return workdir.Resolved{WorkdirID: "p1", TargetID: workspace.WorkspaceTargetNative, Kind: workdir.TargetKindNative, WorkDir: "/data/proj"}
}

// Entries without an end user (schedule, discuss, continuation, runtime
// control, buildBaseRunConfig) resolve both a remote and a native target
// without consulting the permission checker.
func TestResolveWorkspaceWithoutPrincipalSkipsPermissionCheck(t *testing.T) {
	cases := []struct {
		name         string
		workdir      *workdir.Resolved
		requested    string
		wantTargetID string
		wantKind     string
	}{
		{name: "remote workdir", workdir: ptrTo(remoteWorkdir()), wantTargetID: "computer-b", wantKind: workspace.WorkspaceTargetRemote},
		{name: "native workdir", workdir: ptrTo(nativeWorkdir()), wantTargetID: workspace.WorkspaceTargetNative, wantKind: workspace.WorkspaceTargetNative},
		{name: "remote requested", requested: "computer-b", wantTargetID: "computer-b", wantKind: workspace.WorkspaceTargetRemote},
		{name: "default", wantTargetID: workspace.WorkspaceTargetNative, wantKind: workspace.WorkspaceTargetNative},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			perm := &countingPermission{}
			service := wireComponents(&Service{workspaceTargets: kindedTargetResolver{}, botPermissions: perm})
			if tt.workdir != nil {
				service = workdirBoundService(*tt.workdir)
				service.workspaceTargets = kindedTargetResolver{}
				service.botPermissions = perm
			}
			ctx, got, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", SessionID: "s1", RequestedTargetID: tt.requested})
			if err != nil {
				t.Fatalf("resolveWorkspace: %v", err)
			}
			if perm.calls != 0 {
				t.Fatalf("permission checker called %d times without a principal", perm.calls)
			}
			if got.Target == nil || got.Target.TargetID != tt.wantTargetID || got.Target.Kind != tt.wantKind {
				t.Fatalf("target = %#v, want %s/%s", got.Target, tt.wantTargetID, tt.wantKind)
			}
			if pinned := workspace.WorkspaceTargetFromContext(ctx); pinned != tt.wantTargetID {
				t.Fatalf("context target = %q, want %q", pinned, tt.wantTargetID)
			}
			if tt.workdir != nil && got.WorkdirPath != tt.workdir.WorkDir {
				t.Fatalf("workdir path = %q, want %q", got.WorkdirPath, tt.workdir.WorkDir)
			}
		})
	}
}

// An end user reaching a remote computer, by request or by workdir binding,
// is checked against workspace_read; a native default is not.
func TestResolveWorkspaceWithPrincipalChecksRemoteTargets(t *testing.T) {
	principal := &workspacePrincipal{UserID: "user-1"}

	t.Run("remote request denied", func(t *testing.T) {
		perm := &countingPermission{allowed: false}
		service := wireComponents(&Service{workspaceTargets: kindedTargetResolver{}, botPermissions: perm})
		_, _, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", RequestedTargetID: "computer-b", Principal: principal})
		if err == nil || !strings.Contains(err.Error(), "workspace_read") || perm.calls != 1 {
			t.Fatalf("err = %v, calls = %d, want workspace_read denial after one check", err, perm.calls)
		}
	})
	t.Run("remote workdir denied", func(t *testing.T) {
		perm := &countingPermission{allowed: false}
		service := workdirBoundService(remoteWorkdir())
		service.workspaceTargets = kindedTargetResolver{}
		service.botPermissions = perm
		_, _, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", SessionID: "s1", Principal: principal})
		if err == nil || !strings.Contains(err.Error(), "workspace_read") {
			t.Fatalf("err = %v, want workspace_read denial", err)
		}
	})
	t.Run("unidentified principal rejected", func(t *testing.T) {
		service := wireComponents(&Service{workspaceTargets: kindedTargetResolver{}, botPermissions: &countingPermission{allowed: true}})
		_, _, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", RequestedTargetID: "computer-b", Principal: &workspacePrincipal{}})
		if err == nil || !strings.Contains(err.Error(), "user id") {
			t.Fatalf("err = %v, want user id required", err)
		}
	})
	t.Run("native default needs no permission", func(t *testing.T) {
		perm := &countingPermission{}
		service := wireComponents(&Service{workspaceTargets: kindedTargetResolver{}, botPermissions: perm})
		_, got, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", Principal: principal})
		if err != nil || perm.calls != 0 || got.Target == nil || got.Target.Kind != workspace.WorkspaceTargetNative {
			t.Fatalf("err = %v, calls = %d, target = %#v", err, perm.calls, got.Target)
		}
	})
}

func TestResolveWorkspaceBestEffortToleratesOnlyAnUnselectedTarget(t *testing.T) {
	boom := errors.New("boom")
	service := wireComponents(&Service{workspaceTargets: kindedTargetResolver{err: boom}})

	if _, got, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", BestEffort: true}); err != nil || got.Target != nil {
		t.Fatalf("unselected default: err = %v, target = %#v, want tolerated with no target", err, got.Target)
	}
	if _, _, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1"}); !errors.Is(err, boom) {
		t.Fatalf("strict default: err = %v, want boom", err)
	}
	if _, _, err := service.resolveWorkspace(t.Context(), workspaceInput{BotID: "bot-1", RequestedTargetID: "computer-b", BestEffort: true}); !errors.Is(err, boom) {
		t.Fatalf("selected target: err = %v, want boom", err)
	}
	pinned := workspace.WithWorkspaceTarget(t.Context(), "computer-b")
	if _, _, err := service.resolveWorkspace(pinned, workspaceInput{BotID: "bot-1", BestEffort: true}); !errors.Is(err, boom) {
		t.Fatalf("pinned context: err = %v, want boom", err)
	}
}

// A scheduled run acts for the bot's owner, so it binds without a principal,
// and the request that results carries the target its history rows record.
func TestBindRequestWorkspaceWithoutPrincipalRecordsTargetMetadata(t *testing.T) {
	perm := &countingPermission{}
	service := workdirBoundService(remoteWorkdir())
	service.workspaceTargets = kindedTargetResolver{}
	service.botPermissions = perm

	ctx, req, err := service.bindRequestWorkspace(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "owner-1"}, nil, true)
	if err != nil {
		t.Fatalf("bindRequestWorkspace: %v", err)
	}
	if perm.calls != 0 {
		t.Fatalf("permission checked %d times for a principal-less entry", perm.calls)
	}
	if req.WorkspaceTarget == nil || req.WorkspaceTarget.TargetID != "computer-b" || req.WorkspaceTargetID != "computer-b" {
		t.Fatalf("request target = %#v / %q", req.WorkspaceTarget, req.WorkspaceTargetID)
	}
	meta := workspaceTargetMetadata(req.WorkspaceTarget)
	if len(meta) == 0 {
		t.Fatal("history metadata carries no workspace target")
	}
	if workspace.WorkspaceTargetFromContext(ctx) != "computer-b" {
		t.Fatalf("context target = %q", workspace.WorkspaceTargetFromContext(ctx))
	}
}

func TestRuntimeControlContextPinsWorkdirTargetWithoutPrincipal(t *testing.T) {
	perm := &countingPermission{}
	service := workdirBoundService(remoteWorkdir())
	service.workspaceTargets = kindedTargetResolver{}
	service.botPermissions = perm

	ctx, err := service.runtimeControlContext(t.Context(), RuntimeControlRequest{BotID: "bot-1", ThreadID: "s1"})
	if err != nil {
		t.Fatalf("runtimeControlContext: %v", err)
	}
	if got := workspace.WorkspaceTargetFromContext(ctx); got != "computer-b" || perm.calls != 0 {
		t.Fatalf("context target = %q, permission calls = %d", got, perm.calls)
	}
}

// The native schedule entry resolves the workspace itself, ahead of resolve,
// so a bound target that cannot be reached fails the fire instead of running
// in the wrong place.
func TestNativeSchedulePlanBindsWorkspaceBeforeResolve(t *testing.T) {
	boom := errors.New("target unreachable")
	service := workdirBoundService(remoteWorkdir())
	service.workspaceTargets = kindedTargetResolver{err: boom}

	_, err := service.nativeSchedulePlan(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "owner-1"}, schedule.TriggerPayload{OwnerUserID: "owner-1"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the workspace resolution failure", err)
	}
}
