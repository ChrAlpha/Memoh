package discord

import (
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/memohai/memoh/internal/channel"
)

func (a *DiscordAdapter) buildDiscordEditInbound(m *discordgo.MessageUpdate, cfg channel.ChannelConfig) (channel.InboundMessage, bool) {
	if m == nil || m.Message == nil {
		return channel.InboundMessage{}, false
	}
	if m.Author == nil || m.Author.Bot {
		return channel.InboundMessage{}, false
	}
	if m.EditedTimestamp == nil {
		return channel.InboundMessage{}, false
	}
	text := strings.TrimSpace(m.Content)
	attachments := a.collectAttachments(m.Message)
	if text == "" && len(attachments) == 0 {
		return channel.InboundMessage{}, false
	}
	chatType := channel.ConversationTypePrivate
	if m.GuildID != "" {
		chatType = channel.ConversationTypeGroup
	}
	msg := channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID:          m.ID,
			Format:      channel.MessageFormatPlain,
			Text:        text,
			Attachments: attachments,
		},
		BotID:       cfg.BotID,
		ReplyTarget: m.ChannelID,
		Sender: channel.Identity{
			SubjectID:   m.Author.ID,
			DisplayName: m.Author.Username,
			Attributes: map[string]string{
				"user_id":  m.Author.ID,
				"username": m.Author.Username,
			},
		},
		Conversation: channel.Conversation{
			ID:   m.ChannelID,
			Type: chatType,
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "discord",
		Metadata: map[string]any{
			"event_type": "edit",
			"guild_id":   m.GuildID,
		},
	}
	if parts := parseDiscordMessageParts(text); len(parts) > 0 {
		msg.Message.Parts = parts
		msg.Message.Format = channel.MessageFormatRich
	}
	return msg, true
}

func discordEditDedupKey(m *discordgo.MessageUpdate) string {
	if m == nil || m.Message == nil || m.EditedTimestamp == nil || m.ID == "" {
		return ""
	}
	return m.ID + ":edit:" + m.EditedTimestamp.UTC().Format(time.RFC3339Nano)
}

func discordDeleteDedupKey(m *discordgo.MessageDelete) string {
	if m == nil || m.Message == nil || strings.TrimSpace(m.ID) == "" {
		return ""
	}
	return m.ID + ":delete"
}

func buildDiscordDeleteInbound(m *discordgo.MessageDelete, cfg channel.ChannelConfig) (channel.InboundMessage, bool) {
	if m == nil || m.Message == nil {
		return channel.InboundMessage{}, false
	}
	if strings.TrimSpace(m.ID) == "" {
		return channel.InboundMessage{}, false
	}
	chatType := channel.ConversationTypePrivate
	if m.GuildID != "" {
		chatType = channel.ConversationTypeGroup
	}
	return channel.InboundMessage{
		Channel: Type,
		Message: channel.Message{
			ID: m.ID,
		},
		BotID:       cfg.BotID,
		ReplyTarget: m.ChannelID,
		Conversation: channel.Conversation{
			ID:   m.ChannelID,
			Type: chatType,
		},
		ReceivedAt: time.Now().UTC(),
		Source:     "discord",
		Metadata: map[string]any{
			"event_type": "delete",
			"guild_id":   m.GuildID,
		},
	}, true
}
