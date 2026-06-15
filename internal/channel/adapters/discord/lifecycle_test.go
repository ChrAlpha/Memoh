package discord

import (
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/memohai/memoh/internal/channel"
)

func TestBuildDiscordEditInbound_FlagsEventType(t *testing.T) {
	t.Parallel()

	ts := time.Now()
	now := &ts
	mu := &discordgo.MessageUpdate{
		Message: &discordgo.Message{
			ID:              "m1",
			ChannelID:       "c1",
			GuildID:         "g1",
			Content:         "edited body",
			EditedTimestamp: now,
			Author:          &discordgo.User{ID: "u1", Username: "alice"},
		},
	}
	adapter := &DiscordAdapter{}
	msg, ok := adapter.buildDiscordEditInbound(mu, channel.ChannelConfig{BotID: "bot"})
	if !ok {
		t.Fatal("expected ok")
	}
	if got, _ := msg.Metadata["event_type"].(string); got != "edit" {
		t.Fatalf("expected event_type=edit, got %#v", msg.Metadata["event_type"])
	}
	if msg.Message.ID != "m1" {
		t.Fatalf("expected message id m1, got %q", msg.Message.ID)
	}
	if msg.Message.Text != "edited body" {
		t.Fatalf("expected new content, got %q", msg.Message.Text)
	}
	if msg.Conversation.Type != channel.ConversationTypeGroup {
		t.Fatalf("expected group conversation, got %q", msg.Conversation.Type)
	}
}

func TestBuildDiscordEditInbound_PrivateConversationWithoutGuild(t *testing.T) {
	t.Parallel()

	ts := time.Now()
	now := &ts
	mu := &discordgo.MessageUpdate{
		Message: &discordgo.Message{
			ID:              "m1",
			ChannelID:       "c1",
			Content:         "edited",
			EditedTimestamp: now,
			Author:          &discordgo.User{ID: "u1", Username: "alice"},
		},
	}
	adapter := &DiscordAdapter{}
	msg, ok := adapter.buildDiscordEditInbound(mu, channel.ChannelConfig{})
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.Conversation.Type != channel.ConversationTypePrivate {
		t.Fatalf("expected private, got %q", msg.Conversation.Type)
	}
}

func TestBuildDiscordEditInbound_RejectsBotAuthor(t *testing.T) {
	t.Parallel()

	ts := time.Now()
	now := &ts
	mu := &discordgo.MessageUpdate{
		Message: &discordgo.Message{
			ID:              "m1",
			ChannelID:       "c1",
			Content:         "x",
			EditedTimestamp: now,
			Author:          &discordgo.User{ID: "b1", Bot: true},
		},
	}
	adapter := &DiscordAdapter{}
	if _, ok := adapter.buildDiscordEditInbound(mu, channel.ChannelConfig{}); ok {
		t.Fatal("expected bot author to be rejected")
	}
}

func TestBuildDiscordEditInbound_RejectsWhenEditedTimestampNil(t *testing.T) {
	t.Parallel()

	mu := &discordgo.MessageUpdate{
		Message: &discordgo.Message{
			ID:        "m1",
			ChannelID: "c1",
			Content:   "x",
			Author:    &discordgo.User{ID: "u1", Username: "alice"},
		},
	}
	adapter := &DiscordAdapter{}
	if _, ok := adapter.buildDiscordEditInbound(mu, channel.ChannelConfig{}); ok {
		t.Fatal("expected updates without EditedTimestamp to be rejected (embed unfurl / bookkeeping noise)")
	}
}

func TestBuildDiscordEditInbound_RejectsEmptyContentAndNoAttachments(t *testing.T) {
	t.Parallel()

	ts := time.Now()
	now := &ts
	mu := &discordgo.MessageUpdate{
		Message: &discordgo.Message{
			ID:              "m1",
			ChannelID:       "c1",
			EditedTimestamp: now,
			Author:          &discordgo.User{ID: "u1", Username: "alice"},
		},
	}
	adapter := &DiscordAdapter{}
	if _, ok := adapter.buildDiscordEditInbound(mu, channel.ChannelConfig{}); ok {
		t.Fatal("expected empty edit to be rejected")
	}
}

func TestBuildDiscordDeleteInbound_BasicShape(t *testing.T) {
	t.Parallel()

	md := &discordgo.MessageDelete{
		Message: &discordgo.Message{
			ID:        "m1",
			ChannelID: "c1",
			GuildID:   "g1",
		},
	}
	msg, ok := buildDiscordDeleteInbound(md, channel.ChannelConfig{BotID: "bot"})
	if !ok {
		t.Fatal("expected ok")
	}
	if got, _ := msg.Metadata["event_type"].(string); got != "delete" {
		t.Fatalf("expected event_type=delete, got %#v", msg.Metadata["event_type"])
	}
	if msg.Message.ID != "m1" {
		t.Fatalf("expected message id m1, got %q", msg.Message.ID)
	}
	if msg.Conversation.Type != channel.ConversationTypeGroup {
		t.Fatalf("expected group, got %q", msg.Conversation.Type)
	}
	if msg.ReplyTarget != "c1" {
		t.Fatalf("expected reply target = channel id, got %q", msg.ReplyTarget)
	}
}

func TestBuildDiscordDeleteInbound_RejectsEmptyID(t *testing.T) {
	t.Parallel()

	md := &discordgo.MessageDelete{
		Message: &discordgo.Message{ChannelID: "c1"},
	}
	if _, ok := buildDiscordDeleteInbound(md, channel.ChannelConfig{}); ok {
		t.Fatal("expected delete without ID to be rejected")
	}
}
