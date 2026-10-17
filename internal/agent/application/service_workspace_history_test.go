package application

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/bots"
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

func TestPrepareWorkspaceRequestRequiresWorkspaceRead(t *testing.T) {
	base := runreq.ChatRequest{BotID: "bot-1", WorkspaceTargetID: "computer-b"}

	resolver := wireComponents(&Service{workspaceTargets: workspaceRequestTargetService{}})
	if _, _, err := resolver.bindRequestWorkspace(t.Context(), base, userPrincipal(base), false); err == nil || !strings.Contains(err.Error(), "user id") {
		t.Fatalf("missing user error = %v", err)
	}

	base.UserID = "user-1"
	if _, _, err := resolver.bindRequestWorkspace(t.Context(), base, userPrincipal(base), false); err == nil || !strings.Contains(err.Error(), "permission checker") {
		t.Fatalf("missing checker error = %v", err)
	}

	resolver.botPermissions = workspaceRequestPermission(false)
	if _, _, err := resolver.bindRequestWorkspace(t.Context(), base, userPrincipal(base), false); err == nil || !strings.Contains(err.Error(), "workspace_read") {
		t.Fatalf("denied permission error = %v", err)
	}

	resolver.botPermissions = workspaceRequestPermission(true)
	ctx, got, err := resolver.bindRequestWorkspace(t.Context(), base, userPrincipal(base), false)
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

// assertGovernedWorkspaceRuns fails when any kept message carrying an
// execution location is not preceded by a marker for that location.
