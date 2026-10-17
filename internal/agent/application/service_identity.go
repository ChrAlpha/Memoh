package application

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
)

// resolveDisplayName returns the best available display name for the request identity.
func (s *Service) resolveDisplayName(ctx context.Context, req runreq.ChatRequest) string {
	if name := strings.TrimSpace(req.DisplayName); name != "" {
		return name
	}
	if s.queries == nil {
		return "User"
	}
	channelIdentityID := strings.TrimSpace(req.SourceChannelIdentityID)
	if channelIdentityID == "" {
		return "User"
	}
	pgID, err := runreq.ParseServiceUUID(channelIdentityID)
	if err != nil {
		return "User"
	}
	ci, err := s.queries.GetChannelIdentityByID(ctx, pgID)
	if err == nil && ci.DisplayName.Valid {
		if name := strings.TrimSpace(ci.DisplayName.String); name != "" {
			return name
		}
	}
	return "User"
}

func (s *Service) isExistingChannelIdentityID(ctx context.Context, id string) bool {
	if s.queries == nil {
		return false
	}
	pgID, err := runreq.ParseServiceUUID(id)
	if err != nil {
		return false
	}
	_, err = s.queries.GetChannelIdentityByID(ctx, pgID)
	return err == nil
}

func (s *Service) isExistingUserID(ctx context.Context, id string) bool {
	if s.queries == nil {
		return false
	}
	pgID, err := runreq.ParseServiceUUID(id)
	if err != nil {
		return false
	}
	_, err = s.queries.GetUserByID(ctx, pgID)
	return err == nil
}
