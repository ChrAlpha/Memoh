package slack

import (
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/memohai/memoh/internal/channel"
)

func TestBuildSlackEditInbound_FlagsEventTypeAndPreservesOriginalTS(t *testing.T) {
	t.Parallel()
	ev := &slackevents.MessageEvent{
		SubType:     "message_changed",
		Channel:     "C1",
		ChannelType: "channel",
		TimeStamp:   "1700.001",
		Message: &slack.Msg{
			User:      "U1",
			Text:      "edited body",
			Timestamp: "1700.000",
		},
	}
	msg, ok := buildSlackEditInbound(ev, channel.ChannelConfig{BotID: "bot"})
	if !ok {
		t.Fatal("expected ok")
	}
	if got, _ := msg.Metadata["event_type"].(string); got != "edit" {
		t.Fatalf("expected event_type=edit, got %#v", msg.Metadata["event_type"])
	}
	if msg.Message.ID != "1700.000" {
		t.Fatalf("expected message ID = original ts, got %q", msg.Message.ID)
	}
	if msg.Message.Text != "edited body" {
		t.Fatalf("expected new text, got %q", msg.Message.Text)
	}
	if msg.Sender.SubjectID != "U1" {
		t.Fatalf("expected sender from edited message, got %q", msg.Sender.SubjectID)
	}
	if msg.ReplyTarget != "C1" {
		t.Fatalf("expected reply target = channel, got %q", msg.ReplyTarget)
	}
}

func TestBuildSlackEditInbound_PopulatesRichParts(t *testing.T) {
	t.Parallel()
	ev := &slackevents.MessageEvent{
		SubType:     "message_changed",
		Channel:     "C1",
		ChannelType: "channel",
		Message: &slack.Msg{
			User:      "U1",
			Text:      "now *bold*",
			Timestamp: "1700.000",
		},
	}
	msg, ok := buildSlackEditInbound(ev, channel.ChannelConfig{})
	if !ok {
		t.Fatal("expected ok")
	}
	if len(msg.Message.Parts) == 0 {
		t.Fatalf("expected rich parts populated, got %+v", msg.Message)
	}
	if msg.Message.Format != channel.MessageFormatRich {
		t.Fatalf("expected rich format, got %q", msg.Message.Format)
	}
}

func TestBuildSlackEditInbound_RejectsMissingMessage(t *testing.T) {
	t.Parallel()
	ev := &slackevents.MessageEvent{SubType: "message_changed", Channel: "C1"}
	if _, ok := buildSlackEditInbound(ev, channel.ChannelConfig{}); ok {
		t.Fatal("expected false when ev.Message is nil")
	}
}

func TestBuildSlackEditInbound_RejectsEmptyText(t *testing.T) {
	t.Parallel()
	ev := &slackevents.MessageEvent{
		SubType: "message_changed",
		Channel: "C1",
		Message: &slack.Msg{User: "U1", Timestamp: "1700.000"},
	}
	if _, ok := buildSlackEditInbound(ev, channel.ChannelConfig{}); ok {
		t.Fatal("expected false when edited text is empty")
	}
}

func TestBuildSlackEditInbound_GroupVsPrivateChannelType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		channelType string
		want        string
	}{
		{"im", channel.ConversationTypePrivate},
		{"mpim", channel.ConversationTypeGroup},
		{"channel", channel.ConversationTypeGroup},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.channelType, func(t *testing.T) {
			t.Parallel()
			ev := &slackevents.MessageEvent{
				SubType:     "message_changed",
				Channel:     "C1",
				ChannelType: tc.channelType,
				Message:     &slack.Msg{User: "U1", Text: "x", Timestamp: "1700.000"},
			}
			msg, ok := buildSlackEditInbound(ev, channel.ChannelConfig{})
			if !ok {
				t.Fatal("expected ok")
			}
			if msg.Conversation.Type != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, msg.Conversation.Type)
			}
		})
	}
}

func TestBuildSlackDeleteInbound_FlagsEventTypeAndUsesDeletedTS(t *testing.T) {
	t.Parallel()
	ev := &slackevents.MessageEvent{
		SubType:          "message_deleted",
		Channel:          "C1",
		ChannelType:      "channel",
		DeletedTimeStamp: "1700.000",
	}
	msg, ok := buildSlackDeleteInbound(ev, channel.ChannelConfig{BotID: "bot"})
	if !ok {
		t.Fatal("expected ok")
	}
	if got, _ := msg.Metadata["event_type"].(string); got != "delete" {
		t.Fatalf("expected event_type=delete, got %#v", msg.Metadata["event_type"])
	}
	if msg.Message.ID != "1700.000" {
		t.Fatalf("expected message ID = deleted ts, got %q", msg.Message.ID)
	}
	if msg.ReplyTarget != "C1" {
		t.Fatalf("expected reply target = channel, got %q", msg.ReplyTarget)
	}
}

func TestBuildSlackDeleteInbound_RejectsMissingDeletedTS(t *testing.T) {
	t.Parallel()
	ev := &slackevents.MessageEvent{SubType: "message_deleted", Channel: "C1"}
	if _, ok := buildSlackDeleteInbound(ev, channel.ChannelConfig{}); ok {
		t.Fatal("expected false when DeletedTimeStamp is empty")
	}
}
