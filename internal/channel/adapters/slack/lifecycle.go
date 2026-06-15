package slack

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/slack-go/slack/slackevents"

	"github.com/memohai/memoh/internal/channel"
)

func (a *SlackAdapter) dispatchSlackInbound(ctx context.Context, cfg channel.ChannelConfig, handler channel.InboundHandler, msg channel.InboundMessage) {
	go func() {
		if err := handler(ctx, cfg, msg); err != nil && a.logger != nil {
			a.logger.Error("handle inbound failed", slog.String("config_id", cfg.ID), slog.Any("error", err))
		}
	}()
}

func slackChatType(channelType string) string {
	switch channelType {
	case "im":
		return channel.ConversationTypePrivate
	default:
		return channel.ConversationTypeGroup
	}
}

func buildSlackEditInbound(ev *slackevents.MessageEvent, cfg channel.ChannelConfig) (channel.InboundMessage, bool) {
	if ev == nil || ev.Message == nil {
		return channel.InboundMessage{}, false
	}
	text := strings.TrimSpace(ev.Message.Text)
	if text == "" {
		return channel.InboundMessage{}, false
	}
	ts := strings.TrimSpace(ev.Message.Timestamp)
	if ts == "" {
		return channel.InboundMessage{}, false
	}
	user := strings.TrimSpace(ev.Message.User)
	return channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID:     ts,
			Format: channel.MessageFormatPlain,
			Text:   text,
		},
		BotID:       cfg.BotID,
		ReplyTarget: ev.Channel,
		Sender: channel.Identity{
			SubjectID:  user,
			Attributes: slackIdentityAttributes(user, "", ev.ChannelType, ev.Channel),
		},
		Conversation: channel.Conversation{
			ID:   ev.Channel,
			Type: slackChatType(ev.ChannelType),
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "slack",
		Metadata: map[string]any{
			"event_type":   "edit",
			"channel_type": ev.ChannelType,
			"event_ts":     ev.TimeStamp,
		},
	}, true
}

func buildSlackDeleteInbound(ev *slackevents.MessageEvent, cfg channel.ChannelConfig) (channel.InboundMessage, bool) {
	if ev == nil {
		return channel.InboundMessage{}, false
	}
	ts := strings.TrimSpace(ev.DeletedTimeStamp)
	if ts == "" {
		return channel.InboundMessage{}, false
	}
	return channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID: ts,
		},
		BotID:       cfg.BotID,
		ReplyTarget: ev.Channel,
		Conversation: channel.Conversation{
			ID:   ev.Channel,
			Type: slackChatType(ev.ChannelType),
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "slack",
		Metadata: map[string]any{
			"event_type":   "delete",
			"channel_type": ev.ChannelType,
			"event_ts":     ev.TimeStamp,
		},
	}, true
}
