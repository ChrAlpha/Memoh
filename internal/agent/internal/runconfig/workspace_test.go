package runconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

type workspaceRequestTargetService struct{}

func (workspaceRequestTargetService) ResolveWorkspaceTarget(_ context.Context, _ string, targetID string) (workspace.ResolvedWorkspaceTarget, error) {
	return workspace.ResolvedWorkspaceTarget{
		TargetID: strings.TrimSpace(targetID),
		Kind:     workspace.WorkspaceTargetRemote,
		Name:     "Computer B",
	}, nil
}

type workspaceRequestPermission bool

func (allowed workspaceRequestPermission) HasBotPermission(_ context.Context, _, _, permission string) (bool, error) {
	return bool(allowed) && permission == bots.PermissionWorkspaceRead, nil
}

func ptrTo[T any](v T) *T { return &v }

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
		{name: "remote workdir", workdir: ptrTo(agenttest.RemoteWorkdir()), wantTargetID: "computer-b", wantKind: workspace.WorkspaceTargetRemote},
		{name: "native workdir", workdir: ptrTo(agenttest.NativeWorkdir()), wantTargetID: workspace.WorkspaceTargetNative, wantKind: workspace.WorkspaceTargetNative},
		{name: "remote requested", requested: "computer-b", wantTargetID: "computer-b", wantKind: workspace.WorkspaceTargetRemote},
		{name: "default", wantTargetID: workspace.WorkspaceTargetNative, wantKind: workspace.WorkspaceTargetNative},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			perm := &agenttest.CountingPermission{}
			service := newBuilder(func(d *Deps) {
				d.WorkspaceTargets = agenttest.KindedTargetResolver{}
				d.BotPermissions = perm
			})
			if tt.workdir != nil {
				service = workdirBound(*tt.workdir)
				service.targets = agenttest.KindedTargetResolver{}
				service.permissions = perm
			}
			ctx, got, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", SessionID: "s1", RequestedTargetID: tt.requested})
			if err != nil {
				t.Fatalf("resolveWorkspace: %v", err)
			}
			if perm.Calls != 0 {
				t.Fatalf("permission checker called %d times without a principal", perm.Calls)
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
	principal := &WorkspacePrincipal{UserID: "user-1"}

	t.Run("remote request denied", func(t *testing.T) {
		perm := &agenttest.CountingPermission{Allowed: false}
		service := newBuilder(func(d *Deps) {
			d.WorkspaceTargets = agenttest.KindedTargetResolver{}
			d.BotPermissions = perm
		})
		_, _, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", RequestedTargetID: "computer-b", Principal: principal})
		if err == nil || !strings.Contains(err.Error(), "workspace_read") || perm.Calls != 1 {
			t.Fatalf("err = %v, calls = %d, want workspace_read denial after one check", err, perm.Calls)
		}
	})
	t.Run("remote workdir denied", func(t *testing.T) {
		perm := &agenttest.CountingPermission{Allowed: false}
		service := workdirBound(agenttest.RemoteWorkdir())
		service.targets = agenttest.KindedTargetResolver{}
		service.permissions = perm
		_, _, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", SessionID: "s1", Principal: principal})
		if err == nil || !strings.Contains(err.Error(), "workspace_read") {
			t.Fatalf("err = %v, want workspace_read denial", err)
		}
	})
	t.Run("unidentified principal rejected", func(t *testing.T) {
		service := newBuilder(func(d *Deps) {
			d.WorkspaceTargets = agenttest.KindedTargetResolver{}
			d.BotPermissions = &agenttest.CountingPermission{Allowed: true}
		})
		_, _, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", RequestedTargetID: "computer-b", Principal: &WorkspacePrincipal{}})
		if err == nil || !strings.Contains(err.Error(), "user id") {
			t.Fatalf("err = %v, want user id required", err)
		}
	})
	t.Run("native default needs no permission", func(t *testing.T) {
		perm := &agenttest.CountingPermission{}
		service := newBuilder(func(d *Deps) {
			d.WorkspaceTargets = agenttest.KindedTargetResolver{}
			d.BotPermissions = perm
		})
		_, got, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", Principal: principal})
		if err != nil || perm.Calls != 0 || got.Target == nil || got.Target.Kind != workspace.WorkspaceTargetNative {
			t.Fatalf("err = %v, calls = %d, target = %#v", err, perm.Calls, got.Target)
		}
	})
}

func TestResolveWorkspaceBestEffortToleratesOnlyAnUnselectedTarget(t *testing.T) {
	boom := errors.New("boom")
	service := newBuilder(func(d *Deps) {
		d.WorkspaceTargets = agenttest.KindedTargetResolver{Err: boom}
	})

	if _, got, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", BestEffort: true}); err != nil || got.Target != nil {
		t.Fatalf("unselected default: err = %v, target = %#v, want tolerated with no target", err, got.Target)
	}
	if _, _, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1"}); !errors.Is(err, boom) {
		t.Fatalf("strict default: err = %v, want boom", err)
	}
	if _, _, err := service.ResolveWorkspace(t.Context(), WorkspaceInput{BotID: "bot-1", RequestedTargetID: "computer-b", BestEffort: true}); !errors.Is(err, boom) {
		t.Fatalf("selected target: err = %v, want boom", err)
	}
	pinned := workspace.WithWorkspaceTarget(t.Context(), "computer-b")
	if _, _, err := service.ResolveWorkspace(pinned, WorkspaceInput{BotID: "bot-1", BestEffort: true}); !errors.Is(err, boom) {
		t.Fatalf("pinned context: err = %v, want boom", err)
	}
}

// A scheduled run acts for the bot's owner, so it binds without a principal,
// and the request that results carries the target its history rows record.
func TestBindRequestWorkspaceWithoutPrincipalRecordsTargetMetadata(t *testing.T) {
	perm := &agenttest.CountingPermission{}
	service := workdirBound(agenttest.RemoteWorkdir())
	service.targets = agenttest.KindedTargetResolver{}
	service.permissions = perm

	ctx, req, err := service.BindRequestWorkspace(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "owner-1"}, nil, true)
	if err != nil {
		t.Fatalf("bindRequestWorkspace: %v", err)
	}
	if perm.Calls != 0 {
		t.Fatalf("permission checked %d times for a principal-less entry", perm.Calls)
	}
	if req.WorkspaceTarget == nil || req.WorkspaceTarget.TargetID != "computer-b" || req.WorkspaceTargetID != "computer-b" {
		t.Fatalf("request target = %#v / %q", req.WorkspaceTarget, req.WorkspaceTargetID)
	}
	if workspace.WorkspaceTargetFromContext(ctx) != "computer-b" {
		t.Fatalf("context target = %q", workspace.WorkspaceTargetFromContext(ctx))
	}
}

func TestPrepareWorkspaceRequestRequiresWorkspaceRead(t *testing.T) {
	base := runreq.ChatRequest{BotID: "bot-1", WorkspaceTargetID: "computer-b"}

	resolver := newBuilder(func(d *Deps) {
		d.WorkspaceTargets = workspaceRequestTargetService{}
		d.BotPermissions = workspaceRequestPermission(false)
	})
	if _, _, err := resolver.BindRequestWorkspace(t.Context(), base, UserPrincipal(base), false); err == nil || !strings.Contains(err.Error(), "user id") {
		t.Fatalf("missing user error = %v", err)
	}

	base.UserID = "user-1"
	if _, _, err := resolver.BindRequestWorkspace(t.Context(), base, UserPrincipal(base), false); err == nil || !strings.Contains(err.Error(), "workspace_read") {
		t.Fatalf("denied permission error = %v", err)
	}

	resolver.permissions = workspaceRequestPermission(true)
	ctx, got, err := resolver.BindRequestWorkspace(t.Context(), base, UserPrincipal(base), false)
	if err != nil {
		t.Fatalf("prepare allowed request: %v", err)
	}
	if got.WorkspaceTarget == nil || got.WorkspaceTarget.TargetID != "computer-b" {
		t.Fatalf("workspace snapshot = %#v", got.WorkspaceTarget)
	}
	if targetID := workspace.WorkspaceTargetFromContext(ctx); targetID != "computer-b" {
		t.Fatalf("context target = %q, want computer-b", targetID)
	}
}
