package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
)

// CanDecideToolApproval is the single authorization for answering a tool
// approval; see decide.Decider.CanDecideToolApproval.
func (s *Service) CanDecideToolApproval(ctx context.Context, approval toolapproval.Request, principal decide.ToolApprovalPrincipal) (bool, error) {
	return s.decider.CanDecideToolApproval(ctx, approval, principal)
}

// ViewerCanDecideToolApproval answers CanDecideToolApproval for an approval a
// live frame names only by id.
func (s *Service) ViewerCanDecideToolApproval(ctx context.Context, approvalID string, principal decide.ToolApprovalPrincipal) (bool, error) {
	return s.decider.ViewerCanDecideToolApproval(ctx, approvalID, principal)
}
