package decide

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/felinics/memoh/internal/agent/agentapi"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/db"
)

// CanDecideToolApproval is the single authorization for answering a tool
// approval. Every decision entry (Web WS, HTTP, IM buttons and commands) is
// checked by it when the decision is committed, and the per-viewer
// viewer_can_decide flag is computed by it, so what a client is offered and
// what the server accepts cannot diverge.
//
// Who it admits: the user who started the run (D), holding the grant the
// approval's operation requires, and users who manage the bot (C). Global
// admins pass the manage check through ResolveUserPermissions (A, unchanged).
// Who it stops: a grant holder deciding a run somebody else started — in a
// group chat that member is neither the requester nor the bot's manager (D
// acting for another D). A run with no recorded initiator, admitted before the
// ledger kept one or started by a schedule, is decided by a manager only.
func (d *Decider) CanDecideToolApproval(ctx context.Context, approval toolapproval.Request, principal agentapi.ToolApprovalPrincipal) (bool, error) {
	botID := strings.TrimSpace(approval.BotID)
	actorID := principalSubject(principal)
	if botID == "" || actorID == "" {
		return false, nil
	}
	initiatorID, err := d.runInitiator(ctx, approval.RunID)
	if err != nil {
		return false, err
	}
	if principalIs(principal, initiatorID) {
		if permission, ok := toolApprovalPermission(approval); ok && permission != bots.PermissionManage {
			allowed, err := d.botPermissions.HasBotPermission(ctx, botID, actorID, permission)
			if err != nil || allowed {
				return allowed, err
			}
		}
	}
	return d.botPermissions.HasBotPermission(ctx, botID, actorID, bots.PermissionManage)
}

// ViewerCanDecideToolApproval answers CanDecideToolApproval for an approval a
// live frame names only by id.
func (d *Decider) ViewerCanDecideToolApproval(ctx context.Context, approvalID string, principal agentapi.ToolApprovalPrincipal) (bool, error) {
	approval, err := d.toolApproval.Get(ctx, approvalID)
	if err != nil {
		return false, err
	}
	return d.CanDecideToolApproval(ctx, approval, principal)
}

func (d *Decider) runInitiator(ctx context.Context, runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return "", nil
	}
	id, err := db.ParseUUID(runID)
	if err != nil {
		return "", err
	}
	run, err := d.queries.GetSessionRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read run initiator: %w", err)
	}
	return db.TextToString(run.InitiatorID), nil
}

func isCapabilityManagementApproval(target toolapproval.Request) bool {
	return target.ToolName == agenttools.ToolMCPManage().String() || target.ToolName == agenttools.ToolAppManage().String()
}

// toolApprovalPermission is the grant a run's initiator must hold to answer
// the approval: the same authority as performing the operation directly.
// Capability management changes the bot's configuration, which only a manager
// may do.
func toolApprovalPermission(target toolapproval.Request) (string, bool) {
	if isCapabilityManagementApproval(target) {
		return bots.PermissionManage, true
	}
	switch strings.ToLower(strings.TrimSpace(target.Operation)) {
	case toolapproval.OperationRead:
		return bots.PermissionWorkspaceRead, true
	case toolapproval.OperationWrite:
		return bots.PermissionWorkspaceWrite, true
	case toolapproval.OperationExec:
		return bots.PermissionWorkspaceExec, true
	case toolapproval.OperationPermission:
		// A generic agent permission request (network grant, mode switch)
		// authorizes the agent to act, so it sits at the same authority as
		// running the agent's own commands.
		return bots.PermissionWorkspaceExec, true
	default:
		return "", false
	}
}

func principalSubject(p agentapi.ToolApprovalPrincipal) string {
	return runreq.FirstNonEmpty(p.UserID, p.ChannelIdentityID)
}

func principalIs(p agentapi.ToolApprovalPrincipal, id string) bool {
	id = strings.TrimSpace(id)
	return id != "" && (id == strings.TrimSpace(p.UserID) || id == strings.TrimSpace(p.ChannelIdentityID))
}
