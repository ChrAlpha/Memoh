package admit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/runtimefence"
)

// resumeContext is server-authored execution intent, stored alongside the
// original admission input. It contains no JWTs, provider keys or tool outputs.
// The admission fingerprint still names the original request, unchanged.
type ResumeContext struct {
	Version           int               `json:"version"`
	Query             string            `json:"query"`
	ChatID            string            `json:"chat_id"`
	UserID            string            `json:"user_id"`
	ChannelIdentityID string            `json:"channel_identity_id,omitempty"`
	CurrentChannel    string            `json:"current_channel,omitempty"`
	ReplyTarget       string            `json:"reply_target,omitempty"`
	ConversationType  string            `json:"conversation_type,omitempty"`
	Model             string            `json:"model,omitempty"`
	ReasoningEffort   string            `json:"reasoning_effort,omitempty"`
	WorkspaceTargetID string            `json:"workspace_target_id,omitempty"`
	SessionType       string            `json:"session_type,omitempty"`
	ToolHTTPURL       string            `json:"tool_http_url,omitempty"`
	TokenClaims       map[string]string `json:"token_claims,omitempty"`
	ChatClaims        map[string]string `json:"chat_claims,omitempty"`
	DeadlineAt        *time.Time        `json:"deadline_at,omitempty"`
}

// RecordResume stores the server-authored intent a run is resumed from, once,
// when the run starts. A resumed run and a run without a durable fence store
// nothing.
func (a *Admitter) RecordResume(ctx context.Context, req runreq.ChatRequest) error {
	if a.queries == nil || req.RunID == "" || req.ShutdownResume {
		return nil
	}
	fence, ok := runtimefence.FromContext(ctx)
	if !ok || fence.Token <= 0 {
		return nil
	}
	data := ResumeContext{
		Version: 1, Query: runreq.ModelQueryText(req), ChatID: req.ChatID,
		UserID: req.UserID, ChannelIdentityID: req.SourceChannelIdentityID,
		CurrentChannel: req.CurrentChannel, ReplyTarget: req.ReplyTarget,
		ConversationType: req.ConversationType, Model: req.Model, ReasoningEffort: req.ReasoningEffort,
		WorkspaceTargetID: req.WorkspaceTargetID, SessionType: req.SessionType, ToolHTTPURL: req.ToolHTTPURL,
	}
	var err error
	if data.TokenClaims, err = a.signer.Claims(req.Token); err != nil {
		return err
	}
	if data.ChatClaims, err = a.signer.Claims(req.ChatToken); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		data.DeadlineAt = &deadline
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	count, err := a.queries.SaveSessionRunResumeContext(ctx, sqlc.SaveSessionRunResumeContextParams{
		RunID: db.ParseUUIDOrEmpty(req.RunID), FencingToken: fence.Token, ResumeContext: raw,
	})
	if err != nil {
		return fmt.Errorf("save session resume context: %w", err)
	}
	if count != 1 {
		return sessionruntime.ErrRunOwnershipLost
	}
	return nil
}
