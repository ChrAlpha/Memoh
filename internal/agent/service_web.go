package agent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/runreq"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/agent/web"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// The Web client's sends, retries and edits are the Web runner's.

// AdmitWebRun admits a Web submission.
func (s *Service) AdmitWebRun(ctx context.Context, in agentapi.WebAdmitInput) (agentapi.WebAdmission, error) {
	admission, err := s.web.Admit(ctx, in)
	if err != nil {
		// A typed nil in the interface would read as an admitted run.
		return nil, err
	}
	return admission, nil
}

// RunWebSend executes an admitted Web send.
func (s *Service) RunWebSend(admission agentapi.WebAdmission, in agentapi.WebSendInput, delivery agentapi.RunDelivery) {
	s.web.RunSend(admission.(*web.Admission), chatRequestOf(in), delivery)
}

// RunWebRetry executes an admitted retry of the session's latest turn.
func (s *Service) RunWebRetry(admission agentapi.WebAdmission, input agentapi.WebRetryInput, delivery agentapi.RunDelivery) {
	s.web.RunRetry(admission.(*web.Admission), input, delivery)
}

// RunWebEdit executes an admitted edit of the session's latest turn.
func (s *Service) RunWebEdit(admission agentapi.WebAdmission, input agentapi.WebEditInput, delivery agentapi.RunDelivery) {
	s.web.RunEdit(admission.(*web.Admission), input, delivery)
}

// PrepareRetryLatestTurnOperation validates the replacement target of a retry
// and returns the user message where the replaced turn begins.
func (s *Service) PrepareRetryLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error) {
	return s.web.PrepareRetry(ctx, sessionID, turnID)
}

// PrepareRetryRequestUserTurn projects a retry's persisted request for the
// run admission.
func (s *Service) PrepareRetryRequestUserTurn(ctx context.Context, sessionID, requestMessageID string) (*chatview.UITurn, error) {
	return s.web.PrepareRetryRequestUserTurn(ctx, sessionID, requestMessageID)
}

// PrepareEditLatestTurnOperation validates the replacement target of an edit
// and returns the user message where the old turn begins.
func (s *Service) PrepareEditLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error) {
	return s.web.PrepareEdit(ctx, sessionID, turnID)
}

// ResolveTurnIDForMessage maps a stored message id onto the round that contains it.
func (s *Service) ResolveTurnIDForMessage(ctx context.Context, sessionID, messageID string) (string, error) {
	return s.web.ResolveTurnID(ctx, sessionID, messageID)
}

// ApplyUserMessageHookAndPersistUserTurn applies the user-message hook before
// writing a user turn ahead of agent execution.
func (s *Service) ApplyUserMessageHookAndPersistUserTurn(ctx context.Context, in agentapi.WebSendInput) (agentapi.WebSendInput, messagepkg.Message, error) {
	req, message, err := s.web.PersistUserTurn(ctx, chatRequestOf(in))
	return webSendOf(req), message, err
}

// chatRequestOf is the run request of a Web send: the fields the socket fills
// in, the rest left for the run.
func chatRequestOf(in agentapi.WebSendInput) runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID:                    in.BotID,
		ChatID:                   in.ChatID,
		ThreadID:                 in.ThreadID,
		UserID:                   in.UserID,
		SourceChannelIdentityID:  in.SourceChannelIdentityID,
		ExternalMessageID:        in.ExternalMessageID,
		ConversationType:         in.ConversationType,
		CurrentChannel:           in.CurrentChannel,
		ReplyTarget:              in.ReplyTarget,
		Channels:                 in.Channels,
		Query:                    in.Query,
		ModelQuery:               in.ModelQuery,
		RawQuery:                 in.RawQuery,
		UserVisibleText:          in.UserVisibleText,
		UserMessageKind:          in.UserMessageKind,
		Messages:                 in.Messages,
		SkillActivation:          in.SkillActivation,
		RequestedSkills:          in.RequestedSkills,
		Attachments:              in.Attachments,
		WorkspaceTargetID:        in.WorkspaceTargetID,
		WorkspaceTarget:          in.WorkspaceTarget,
		UserMessagePersisted:     in.UserMessagePersisted,
		PersistedUserMessageID:   in.PersistedUserMessageID,
		Token:                    in.Token,
		ChatToken:                in.ChatToken,
		Model:                    in.Model,
		ReasoningEffort:          in.ReasoningEffort,
		ToolHTTPURL:              in.ToolHTTPURL,
		AgentCommand:             in.AgentCommand,
		SkipMemoryExtraction:     in.SkipMemoryExtraction,
		SkipTitleGeneration:      in.SkipTitleGeneration,
		OnModelPreferenceSettled: in.OnModelPreferenceSettled,
	}
}

// webSendOf is chatRequestOf's inverse: the request as the user-message hook
// left it, narrowed to what the socket reads back.
func webSendOf(req runreq.ChatRequest) agentapi.WebSendInput {
	return agentapi.WebSendInput{
		BotID:                    req.BotID,
		ChatID:                   req.ChatID,
		ThreadID:                 req.ThreadID,
		UserID:                   req.UserID,
		SourceChannelIdentityID:  req.SourceChannelIdentityID,
		ExternalMessageID:        req.ExternalMessageID,
		ConversationType:         req.ConversationType,
		CurrentChannel:           req.CurrentChannel,
		ReplyTarget:              req.ReplyTarget,
		Channels:                 req.Channels,
		Query:                    req.Query,
		ModelQuery:               req.ModelQuery,
		RawQuery:                 req.RawQuery,
		UserVisibleText:          req.UserVisibleText,
		UserMessageKind:          req.UserMessageKind,
		Messages:                 req.Messages,
		SkillActivation:          req.SkillActivation,
		RequestedSkills:          req.RequestedSkills,
		Attachments:              req.Attachments,
		WorkspaceTargetID:        req.WorkspaceTargetID,
		WorkspaceTarget:          req.WorkspaceTarget,
		UserMessagePersisted:     req.UserMessagePersisted,
		PersistedUserMessageID:   req.PersistedUserMessageID,
		Token:                    req.Token,
		ChatToken:                req.ChatToken,
		Model:                    req.Model,
		ReasoningEffort:          req.ReasoningEffort,
		ToolHTTPURL:              req.ToolHTTPURL,
		AgentCommand:             req.AgentCommand,
		SkipMemoryExtraction:     req.SkipMemoryExtraction,
		SkipTitleGeneration:      req.SkipTitleGeneration,
		OnModelPreferenceSettled: req.OnModelPreferenceSettled,
	}
}
