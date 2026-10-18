package botenv

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
)

// ResolveDisplayName returns the best available display name for the request identity.
func (e *Env) ResolveDisplayName(ctx context.Context, req runreq.ChatRequest) string {
	if name := strings.TrimSpace(req.DisplayName); name != "" {
		return name
	}
	channelIdentityID := strings.TrimSpace(req.SourceChannelIdentityID)
	if channelIdentityID == "" {
		return "User"
	}
	pgID, err := runreq.ParseServiceUUID(channelIdentityID)
	if err != nil {
		return "User"
	}
	ci, err := e.queries.GetChannelIdentityByID(ctx, pgID)
	if err == nil && ci.DisplayName.Valid {
		if name := strings.TrimSpace(ci.DisplayName.String); name != "" {
			return name
		}
	}
	return "User"
}

func (e *Env) IsExistingChannelIdentity(ctx context.Context, id string) bool {
	pgID, err := runreq.ParseServiceUUID(id)
	if err != nil {
		return false
	}
	_, err = e.queries.GetChannelIdentityByID(ctx, pgID)
	return err == nil
}

func (e *Env) IsExistingUser(ctx context.Context, id string) bool {
	pgID, err := runreq.ParseServiceUUID(id)
	if err != nil {
		return false
	}
	_, err = e.queries.GetUserByID(ctx, pgID)
	return err == nil
}
