package subagent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	tools "github.com/felinics/memoh/internal/agent/tool"
)

// RecordResume is called after admission and before the first child tool.
func (h *Host) RecordResume(ctx context.Context, session tools.SessionContext, sessionID, query, model string) error {
	handle, ok := HandleFromContext(ctx)
	if !ok {
		return nil
	}
	return h.admit.RecordResume(ctx, runreq.ChatRequest{
		BotID: session.BotID, ThreadID: sessionID, RunID: handle.RunID,
		ChatID: session.ChatID, UserID: session.UserID, SourceChannelIdentityID: session.ChannelIdentityID,
		ChatToken: session.SessionToken, CurrentChannel: session.CurrentPlatform, ReplyTarget: session.ReplyTarget,
		ConversationType: session.ConversationType, WorkspaceTargetID: session.WorkspaceTargetID, Query: query, Model: model, SessionType: "subagent",
	})
}
