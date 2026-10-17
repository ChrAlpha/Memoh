package application

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// ApplyUserMessageHookAndPersistUserTurn applies the normal user-message hook
// before writing a user turn ahead of agent execution. Web skill activations
// use this so a denied hook cannot leave a persisted user-only special turn.
func (s *Service) ApplyUserMessageHookAndPersistUserTurn(ctx context.Context, req runreq.ChatRequest) (runreq.ChatRequest, messagepkg.Message, error) {
	if req.WorkspaceTarget == nil {
		var err error
		ctx, req, err = s.bindRequestWorkspace(ctx, req, userPrincipal(req), false)
		if err != nil {
			return req, messagepkg.Message{}, err
		}
	}
	if req.RawQuery == "" {
		req.RawQuery = strings.TrimSpace(req.Query)
	}
	nextReq, err := s.botenv.ApplyUserMessageHook(ctx, req)
	if err != nil {
		return req, messagepkg.Message{}, err
	}
	persisted, err := s.persist.UserTurn(ctx, nextReq)
	if err != nil {
		return nextReq, messagepkg.Message{}, err
	}
	nextReq.UserMessagePersisted = true
	nextReq.PersistedUserMessageID = persisted.ID
	return nextReq, persisted, nil
}
