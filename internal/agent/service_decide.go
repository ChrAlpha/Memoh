package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
)

// CanDecideToolApproval is the single authorization for answering a tool
// approval: the initiator holding the operation's own grant, or a manager of
// the bot. ViewerCanDecideToolApproval answers the same question for display.
func (s *Service) CanDecideToolApproval(ctx context.Context, approval toolapproval.Request, principal agentapi.ToolApprovalPrincipal) (bool, error) {
	return s.decider.CanDecideToolApproval(ctx, approval, principal)
}

// ViewerCanDecideToolApproval answers CanDecideToolApproval for an approval a
// live frame names only by id.
func (s *Service) ViewerCanDecideToolApproval(ctx context.Context, approvalID string, principal agentapi.ToolApprovalPrincipal) (bool, error) {
	return s.decider.ViewerCanDecideToolApproval(ctx, approvalID, principal)
}
