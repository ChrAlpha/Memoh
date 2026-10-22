package runconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

type sessionWorkdirSessionService struct {
	thread sessionpkg.Thread
}

func (s sessionWorkdirSessionService) Get(context.Context, string) (sessionpkg.Thread, error) {
	return s.thread, nil
}

func (s sessionWorkdirSessionService) UpdateTitle(context.Context, string, string) (sessionpkg.Thread, error) {
	return s.thread, nil
}

func (s sessionWorkdirSessionService) UpdateMetadata(context.Context, string, map[string]any) (sessionpkg.Thread, error) {
	return s.thread, nil
}

func (s sessionWorkdirSessionService) MergeRuntimeMetadata(context.Context, string, string, map[string]any) (sessionpkg.Thread, error) {
	return s.thread, nil
}

type fakeSessionWorkdirResolver struct {
	resolved workdir.Resolved
	err      error
}

func (f fakeSessionWorkdirResolver) ResolveForSession(context.Context, string, string) (workdir.Resolved, error) {
	return f.resolved, f.err
}

// workdirBound is a Builder whose session s1 is bound to the workdir.
func workdirBound(resolved workdir.Resolved) *Builder {
	return newBuilder(func(d *Deps) {
		d.Sessions = sessionWorkdirSessionService{thread: sessionpkg.Thread{ID: "s1", BotID: "bot-1", WorkdirID: "p1"}}
		d.Workdirs = fakeSessionWorkdirResolver{resolved: resolved}
		d.WorkspaceTargets = workspaceRequestTargetService{}
	})
}

func TestPrepareWorkspaceRequestRejectsWorkdirTargetConflict(t *testing.T) {
	service := workdirBound(workdir.Resolved{
		WorkdirID: "p1", TargetID: workspace.WorkspaceTargetNative, Kind: workdir.TargetKindNative, WorkDir: "/data/proj",
	})
	req := runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "user-1", WorkspaceTargetID: "computer-b"}
	if _, _, err := service.BindRequestWorkspace(t.Context(), req, UserPrincipal(req), false); !errors.Is(err, ErrWorkspaceTargetWorkdirConflict) {
		t.Fatalf("error = %v, want ErrWorkspaceTargetWorkdirConflict", err)
	}
}

func TestPrepareWorkspaceRequestInjectsNativeWorkdirTargetWithoutWorkspaceRead(t *testing.T) {
	// A native workdir pins the same workspace every chat session already
	// uses — it must not demand workspace_read just to keep chatting.
	// botPermissions is deliberately nil: any permission check would panic
	// the "checker not configured" branch into an error.
	service := workdirBound(workdir.Resolved{
		WorkdirID: "p1", TargetID: workspace.WorkspaceTargetNative, Kind: workdir.TargetKindNative, WorkDir: "/data/proj",
	})
	ctx, got, err := service.BindRequestWorkspace(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1"}, &WorkspacePrincipal{}, false)
	if err != nil {
		t.Fatalf("prepare error = %v", err)
	}
	if got.WorkspaceTargetID != workspace.WorkspaceTargetNative {
		t.Fatalf("WorkspaceTargetID = %q, want native", got.WorkspaceTargetID)
	}
	if targetID := workspace.WorkspaceTargetFromContext(ctx); targetID != workspace.WorkspaceTargetNative {
		t.Fatalf("context target = %q, want native", targetID)
	}
}

func TestPrepareWorkspaceRequestRemoteWorkdirRequiresWorkspaceRead(t *testing.T) {
	resolved := workdir.Resolved{
		WorkdirID: "p1", TargetID: "computer-b", Kind: workdir.TargetKindRemote, WorkDir: "/Users/alice/code",
	}

	denied := workdirBound(resolved)
	denied.permissions = workspaceRequestPermission(false)
	req := runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "user-1"}
	if _, _, err := denied.BindRequestWorkspace(t.Context(), req, UserPrincipal(req), false); err == nil || !strings.Contains(err.Error(), "workspace_read") {
		t.Fatalf("denied error = %v, want workspace_read denial", err)
	}

	allowed := workdirBound(resolved)
	allowed.permissions = workspaceRequestPermission(true)
	ctx, got, err := allowed.BindRequestWorkspace(t.Context(), req, UserPrincipal(req), false)
	if err != nil {
		t.Fatalf("allowed error = %v", err)
	}
	if got.WorkspaceTargetID != "computer-b" {
		t.Fatalf("WorkspaceTargetID = %q, want computer-b", got.WorkspaceTargetID)
	}
	if targetID := workspace.WorkspaceTargetFromContext(ctx); targetID != "computer-b" {
		t.Fatalf("context target = %q, want computer-b", targetID)
	}
}

func TestPrepareWorkspaceRequestAcceptsMatchingExplicitTarget(t *testing.T) {
	service := workdirBound(workdir.Resolved{
		WorkdirID: "p1", TargetID: "computer-b", Kind: workdir.TargetKindRemote, WorkDir: "/Users/alice/code",
	})
	service.permissions = workspaceRequestPermission(true)
	req := runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1", UserID: "user-1", WorkspaceTargetID: "computer-b"}
	if _, _, err := service.BindRequestWorkspace(t.Context(), req, UserPrincipal(req), false); err != nil {
		t.Fatalf("matching explicit target must pass, got %v", err)
	}
}

func TestResolveSessionWorkdirBindingFailsClosed(t *testing.T) {
	// A resolution failure must fail the turn, not silently degrade to the
	// default workspace: running in the wrong directory is the bug class
	// workdir bindings exist to eliminate.
	service := workdirBound(workdir.Resolved{})
	service.workdirs = sessionWorkdirs{sessions: service.sessions, workdirs: fakeSessionWorkdirResolver{err: errors.New("boom")}}
	if _, _, err := service.BindRequestWorkspace(t.Context(), runreq.ChatRequest{BotID: "bot-1", ThreadID: "s1"}, &WorkspacePrincipal{}, false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want propagated resolution failure", err)
	}
}
