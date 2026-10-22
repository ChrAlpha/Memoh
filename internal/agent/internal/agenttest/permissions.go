package agenttest

import (
	"context"
	"strings"
)

// PermissionChecker answers bot permission checks from a fixed table.
type PermissionChecker struct {
	Values map[string]bool
	err    error
}

func (f *PermissionChecker) HasBotPermission(_ context.Context, botID, accountID, permission string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.Values[strings.TrimSpace(botID)+":"+strings.TrimSpace(accountID)+":"+strings.TrimSpace(permission)], nil
}

// RecordingPermissionChecker answers every check with Allow and keeps the last
// permission it was asked about.
type RecordingPermissionChecker struct {
	Permission string
	Allow      bool
}

func (c *RecordingPermissionChecker) HasBotPermission(_ context.Context, _, _, permission string) (bool, error) {
	c.Permission = permission
	return c.Allow, nil
}
