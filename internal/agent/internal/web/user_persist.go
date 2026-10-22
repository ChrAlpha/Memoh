package web

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/internal/runconfig"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// PersistUserTurn applies the normal user-message hook
// before writing a user turn ahead of agent execution. Web skill activations
// use this so a denied hook cannot leave a persisted user-only special turn.
func (r *Runner) PersistUserTurn(ctx context.Context, req runreq.ChatRequest) (runreq.ChatRequest, messagepkg.Message, error) {
	if req.WorkspaceTarget == nil {
		var err error
		ctx, req, err = r.runconfig.BindRequestWorkspace(ctx, req, runconfig.UserPrincipal(req), false)
		if err != nil {
			return req, messagepkg.Message{}, err
		}
	}
	if req.RawQuery == "" {
		req.RawQuery = strings.TrimSpace(req.Query)
	}
	nextReq, err := r.bot.ApplyUserMessageHook(ctx, req)
	if err != nil {
		return req, messagepkg.Message{}, err
	}
	persisted, err := r.persist.UserTurn(ctx, nextReq)
	if err != nil {
		return nextReq, messagepkg.Message{}, err
	}
	nextReq.UserMessagePersisted = true
	nextReq.PersistedUserMessageID = persisted.ID
	return nextReq, persisted, nil
}
