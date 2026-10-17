package application

import "context"

// ValidateWorkspaceTarget validates a user-selected Computer without changing
// the Bot's Primary target. It is used by handlers before creating a session.
func (s *Service) ValidateWorkspaceTarget(ctx context.Context, botID, targetID string) error {
	return s.runconfig.ValidateWorkspaceTarget(ctx, botID, targetID)
}

// ReconcileSessionModelPreference validates and normalizes a candidate model
// and effort pair for the session preference.
func (s *Service) ReconcileSessionModelPreference(ctx context.Context, botID, modelRef, effort string) (string, string, error) {
	return s.runconfig.ReconcilePreference(ctx, botID, modelRef, effort)
}
