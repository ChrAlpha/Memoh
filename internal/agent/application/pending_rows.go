package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// persistPrecedingMessages writes the earlier messages of a batch as user rows
// of their own, in the order they arrived, before the run that answers them is
// admitted. Each is a message of the conversation like any other: it keeps its
// sender, its mentions, the message it replies to and its attachments, and its
// header is the one a message recorded without a run has. The run's request,
// which is the last of the batch, is written by the run itself, so the model
// reads them as it would had they been recorded as they came.
//
// The rows are written first so they take lower turn positions than the run's
// turn, which admission draws. A row is written under the identity of the queue
// item it waited as, once: a batch that is started again, after a crash or a
// refused admission, finds the rows it wrote.
func (s *Service) persistPrecedingMessages(ctx context.Context, cmd turn.StartTurnCommand) error {
	if len(cmd.Preceding) == 0 {
		return nil
	}
	persister, ok := s.messageService.(messagepkg.UserRowPersister)
	if !ok {
		return errors.New("message service cannot write a batch of user rows")
	}
	rows := make([]messagepkg.UserRow, 0, len(cmd.Preceding))
	for _, earlier := range cmd.Preceding {
		row, err := s.precedingUserRow(ctx, cmd, earlier)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	if _, err := persister.PersistUserRows(ctx, rows); err != nil {
		return fmt.Errorf("persist the earlier messages of a batch: %w", err)
	}
	return nil
}

// precedingRequest is the request the earlier message would have been, said in
// the place the batch's command is: what is the message's own is its own, where
// it was said is the command's.
func precedingRequest(cmd turn.StartTurnCommand, earlier turn.PrecedingMessage) runreq.ChatRequest {
	return runreq.ChatRequest{
		BotID:                     cmd.BotID,
		ChatID:                    cmd.ChatID,
		ThreadID:                  cmd.ThreadID,
		RouteID:                   cmd.RouteID,
		UserID:                    earlier.UserID,
		SourceChannelIdentityID:   earlier.SourceChannelIdentityID,
		DisplayName:               earlier.DisplayName,
		ExternalMessageID:         earlier.ExternalMessageID,
		ReplyTarget:               cmd.ReplyTarget,
		ConversationType:          cmd.ConversationType,
		ConversationName:          cmd.ConversationName,
		CurrentChannel:            cmd.CurrentChannel,
		SourceReplyToMessageID:    earlier.SourceReplyToMessageID,
		ReplySender:               earlier.ReplySender,
		ReplyPreview:              earlier.ReplyPreview,
		ReplyAttachments:          earlier.ReplyAttachments,
		MentionsBot:               earlier.MentionsBot,
		RepliesToBot:              earlier.RepliesToBot,
		ForwardMessageID:          earlier.ForwardMessageID,
		ForwardFromUserID:         earlier.ForwardFromUserID,
		ForwardFromConversationID: earlier.ForwardFromConversationID,
		ForwardSender:             earlier.ForwardSender,
		ForwardDate:               earlier.ForwardDate,
		Attachments:               earlier.Attachments,
		Query:                     earlier.Query,
		UserVisibleText:           earlier.UserVisibleText,
	}
}

func (s *Service) precedingUserRow(ctx context.Context, cmd turn.StartTurnCommand, earlier turn.PrecedingMessage) (messagepkg.UserRow, error) {
	id := strings.TrimSpace(earlier.ID)
	if id == "" {
		return messagepkg.UserRow{}, errors.New("an earlier message of a batch has no identity")
	}
	req := precedingRequest(cmd, earlier)
	text := strings.TrimSpace(earlier.Query)
	if text == "" && len(earlier.Attachments) == 0 {
		return messagepkg.UserRow{}, errors.New("an earlier message of a batch has no text or attachments")
	}

	var paths []string
	for _, att := range earlier.Attachments {
		if path := strings.TrimSpace(att.Path); path != "" {
			paths = append(paths, path)
		}
	}
	at := time.Now().UTC()
	if earlier.ReceivedAtMs > 0 {
		at = time.UnixMilli(earlier.ReceivedAtMs).UTC()
	}
	forwardedFrom := ""
	if req.ForwardMessageID != "" || req.ForwardSender != "" || req.ForwardFromUserID != "" || req.ForwardFromConversationID != "" {
		forwardedFrom = turn.ForwardedFromHeader(req.ForwardSender, req.ForwardFromUserID, req.ForwardFromConversationID)
	}
	header := turn.FormatUserHeader(turn.UserMessageHeaderInput{
		MessageID:          strings.TrimSpace(req.ExternalMessageID),
		ChannelIdentityID:  strings.TrimSpace(req.SourceChannelIdentityID),
		DisplayName:        s.botenv.ResolveDisplayName(ctx, req),
		Channel:            req.CurrentChannel,
		ConversationType:   strings.TrimSpace(req.ConversationType),
		ConversationName:   strings.TrimSpace(req.ConversationName),
		Target:             strings.TrimSpace(req.ReplyTarget),
		AttachmentPaths:    paths,
		Time:               at,
		ForwardedFrom:      forwardedFrom,
		ForwardedMessageID: strings.TrimSpace(req.ForwardMessageID),
		ReplyToMessageID:   strings.TrimSpace(req.SourceReplyToMessageID),
		ReplyToSender:      strings.TrimSpace(req.ReplySender),
		ReplyToPreview:     req.ReplyPreview,
		MentionsMe:         req.MentionsBot,
		RepliesToMe:        req.RepliesToBot,
	}, text)
	content, err := historyfrag.MarshalStoredModelMessage(runreq.NormalizeUserMessageContent(runreq.ModelMessage{
		Role: "user", Content: runreq.NewTextContent(header),
	}))
	if err != nil {
		return messagepkg.UserRow{}, fmt.Errorf("marshal an earlier message of a batch: %w", err)
	}
	displayText := text
	if visible := strings.TrimSpace(earlier.UserVisibleText); visible != "" {
		displayText = visible
	}
	senderChannelIdentityID, senderUserID := s.persist.SenderIDs(ctx, req)
	sessionMode, runtimeType := s.persist.SessionRuntimeSnapshot(ctx, req)
	return messagepkg.UserRow{
		ID: id,
		Input: messagepkg.PersistInput{
			BotID:                   req.BotID,
			SessionID:               req.ThreadID,
			SenderChannelIdentityID: senderChannelIdentityID,
			SenderUserID:            senderUserID,
			ExternalMessageID:       req.ExternalMessageID,
			SourceReplyToMessageID:  req.SourceReplyToMessageID,
			Role:                    "user",
			Content:                 content,
			Metadata:                persist.MergeMetadata(persist.RouteMetadata(req), persist.InteractionMetadata(req)),
			Assets:                  runreq.ChatAttachmentsToAssetRefs(req.Attachments),
			DisplayText:             displayText,
			SessionMode:             sessionMode,
			RuntimeType:             runtimeType,
		},
	}, nil
}
