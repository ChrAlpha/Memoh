package persist

import (
	"context"
	"errors"
	"strings"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/runreq"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// UserTurn writes a user turn before agent execution. It is used after
// the user-message hook has already accepted the request.
func (w *Writer) UserTurn(ctx context.Context, req runreq.ChatRequest) (messagepkg.Message, error) {
	if w.messages == nil {
		return messagepkg.Message{}, errors.New("message service not configured")
	}
	if strings.TrimSpace(req.BotID) == "" {
		return messagepkg.Message{}, errors.New("bot id is required")
	}
	if strings.TrimSpace(req.ThreadID) == "" {
		return messagepkg.Message{}, errors.New("session id is required")
	}
	modelMessage := runreq.ModelMessage{
		Role:    "user",
		Content: runreq.NewTextContent(persistedUserTurnText(req)),
	}
	modelMessage = runreq.NormalizeUserMessageContent(modelMessage)
	content, err := historyfrag.MarshalStoredModelMessage(modelMessage)
	if err != nil {
		return messagepkg.Message{}, err
	}
	displayText := strings.TrimSpace(req.Query)
	if strings.TrimSpace(req.UserVisibleText) != "" || req.UserMessageKind == runreq.UserMessageKindSkillActivation {
		displayText = strings.TrimSpace(req.UserVisibleText)
	} else if strings.TrimSpace(req.RawQuery) != "" {
		displayText = strings.TrimSpace(req.RawQuery)
	}
	senderChannelIdentityID, senderUserID := w.SenderIDs(ctx, req)
	sessionMode, runtimeType := w.SessionRuntimeSnapshot(ctx, req)
	return w.messages.Persist(ctx, messagepkg.PersistInput{
		BotID:                   req.BotID,
		SessionID:               req.ThreadID,
		SenderChannelIdentityID: senderChannelIdentityID,
		SenderUserID:            senderUserID,
		ExternalMessageID:       req.ExternalMessageID,
		SourceReplyToMessageID:  req.SourceReplyToMessageID,
		Role:                    "user",
		Content:                 content,
		Metadata:                MergeMetadata(MergeMetadata(RouteMetadata(req), InteractionMetadata(req)), workspaceTargetMetadata(req.WorkspaceTarget)),
		Assets:                  runreq.ChatAttachmentsToAssetRefs(req.Attachments),
		EventID:                 req.EventID,
		DisplayText:             displayText,
		SessionMode:             sessionMode,
		RuntimeType:             runtimeType,
		RunID:                   req.RunID,
		TurnID:                  req.TurnID,
		TurnPosition:            req.TurnPosition,
	})
}

func persistedUserTurnText(req runreq.ChatRequest) string {
	if req.UserMessageKind == runreq.UserMessageKindSkillActivation {
		return strings.TrimSpace(req.UserVisibleText)
	}
	return req.Query
}
