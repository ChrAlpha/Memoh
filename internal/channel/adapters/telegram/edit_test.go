package telegram

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/memohai/memoh/internal/channel"
)

func TestBuildTelegramEditedInboundMessage_FlagsEventType(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	bot := &tgbotapi.BotAPI{Token: "test", Self: tgbotapi.User{ID: 1, UserName: "memohbot"}}
	update := tgbotapi.Update{
		UpdateID: 4242,
		EditedMessage: &tgbotapi.Message{
			MessageID: 555,
			EditDate:  1710000100,
			Date:      1710000000,
			Text:      "edited body",
			Chat:      &tgbotapi.Chat{ID: 9, Type: "private"},
			From:      &tgbotapi.User{ID: 7, UserName: "alice"},
		},
	}

	inbound, ok := adapter.buildTelegramEditedInboundMessage(bot, channel.ChannelConfig{}, update)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if got, _ := inbound.Metadata["event_type"].(string); got != "edit" {
		t.Fatalf("expected event_type=edit, got %#v", inbound.Metadata["event_type"])
	}
	if inbound.Message.ID != "555" {
		t.Fatalf("expected message_id=555, got %q", inbound.Message.ID)
	}
	if inbound.Message.Text != "edited body" {
		t.Fatalf("expected new text, got %q", inbound.Message.Text)
	}
}

func TestBuildTelegramEditedInboundMessage_PreservesUpdateID(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	bot := &tgbotapi.BotAPI{Token: "test", Self: tgbotapi.User{ID: 1, UserName: "memohbot"}}
	update := tgbotapi.Update{
		UpdateID: 99,
		EditedMessage: &tgbotapi.Message{
			MessageID: 1,
			Text:      "x",
			Chat:      &tgbotapi.Chat{ID: 1, Type: "private"},
			From:      &tgbotapi.User{ID: 1, UserName: "u"},
		},
	}
	inbound, ok := adapter.buildTelegramEditedInboundMessage(bot, channel.ChannelConfig{}, update)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if got := inbound.Metadata["update_id"]; got != 99 {
		t.Fatalf("expected update_id=99, got %#v", got)
	}
}

func TestBuildTelegramEditedInboundMessage_NilEditedReturnsFalse(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	update := tgbotapi.Update{UpdateID: 1}
	if _, ok := adapter.buildTelegramEditedInboundMessage(nil, channel.ChannelConfig{}, update); ok {
		t.Fatal("expected false when EditedMessage is nil")
	}
}

func TestBuildTelegramEditedInboundMessage_PreservesRichParts(t *testing.T) {
	t.Parallel()

	adapter := NewTelegramAdapter(nil)
	bot := &tgbotapi.BotAPI{Token: "test", Self: tgbotapi.User{ID: 1, UserName: "memohbot"}}
	update := tgbotapi.Update{
		UpdateID: 1,
		EditedMessage: &tgbotapi.Message{
			MessageID: 1,
			Text:      "see https://example.com",
			Chat:      &tgbotapi.Chat{ID: 1, Type: "private"},
			From:      &tgbotapi.User{ID: 1, UserName: "u"},
			Entities: []tgbotapi.MessageEntity{
				{Type: "url", Offset: 4, Length: 19},
			},
		},
	}
	inbound, ok := adapter.buildTelegramEditedInboundMessage(bot, channel.ChannelConfig{}, update)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if len(inbound.Message.Parts) == 0 {
		t.Fatalf("expected Parts populated for edited message, got %+v", inbound.Message)
	}
}
