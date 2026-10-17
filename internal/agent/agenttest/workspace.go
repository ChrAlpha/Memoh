package agenttest

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/workdir"
	"github.com/felinics/memoh/internal/workspace"
)

// KindedTargetResolver reports native for the native id and remote otherwise;
// an empty id resolves to the context-pinned target, then to native.
type KindedTargetResolver struct{ Err error }

func (r KindedTargetResolver) ResolveWorkspaceTarget(ctx context.Context, _ string, targetID string) (workspace.ResolvedWorkspaceTarget, error) {
	if r.Err != nil {
		return workspace.ResolvedWorkspaceTarget{}, r.Err
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

// CountingPermission answers every permission check the same and counts them.
type CountingPermission struct {
	Allowed bool
	Calls   int
}

func (c *CountingPermission) HasBotPermission(context.Context, string, string, string) (bool, error) {
	c.Calls++
	return c.Allowed, nil
}

// RemoteWorkdir is a workdir bound to a remote computer.
func RemoteWorkdir() workdir.Resolved {
	return workdir.Resolved{WorkdirID: "p1", TargetID: "computer-b", Kind: workdir.TargetKindRemote, WorkDir: "/Users/alice/code"}
}

// NativeWorkdir is a workdir on the server workspace.
func NativeWorkdir() workdir.Resolved {
	return workdir.Resolved{WorkdirID: "p1", TargetID: workspace.WorkspaceTargetNative, Kind: workdir.TargetKindNative, WorkDir: "/data/proj"}
}
