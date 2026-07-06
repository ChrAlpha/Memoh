package flow

import (
	"strings"
	"testing"
	"time"
)

func TestFormatUserHeaderIncludesAttachments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC)
	header := FormatUserHeader(UserMessageHeaderInput{
		MessageID:         "msg_1",
		ChannelIdentityID: "cid_1",
		DisplayName:       "Alice",
		Channel:           "feishu",
		ConversationType:  "group",
		ConversationName:  "Team Chat",
		AttachmentPaths:   []string{"/tmp/a.txt"},
		Time:              now,
		Timezone:          "UTC",
	}, "hello")

	if !strings.Contains(header, "<attachment path=\"/tmp/a.txt\"/>") {
		t.Fatalf("expected attachment tag in header: %s", header)
	}
}

func TestFormatUserHeaderWithoutAttachmentsUsesEmptyList(t *testing.T) {
	t.Parallel()

	header := FormatUserHeader(UserMessageHeaderInput{
		ChannelIdentityID: "cid_1",
		DisplayName:       "Alice",
		Channel:           "feishu",
		ConversationType:  "group",
		ConversationName:  "Team Chat",
		Time:              time.Now().UTC(),
	}, "hello")

	if strings.Contains(header, "<attachment ") {
		t.Fatalf("expected no attachment tag in header: %s", header)
	}
}

func TestFormatUserHeaderWithoutInteractionMetadataBytesUnchanged(t *testing.T) {
	t.Parallel()

	header := FormatUserHeader(UserMessageHeaderInput{
		MessageID:         "msg_1",
		ChannelIdentityID: "cid_1",
		DisplayName:       "Alice",
		Channel:           "feishu",
		ConversationType:  "group",
		ConversationName:  "Team Chat",
		Target:            "oc_1",
		AttachmentPaths:   []string{"/tmp/a.txt"},
		Time:              time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
		Timezone:          "UTC",
	}, "hello")

	want := "<message id=\"msg_1\" sender=\"Alice\" t=\"2026-04-06T10:00:00Z\" channel=\"feishu\" conversation=\"Team Chat\" type=\"group\" target=\"oc_1\">\n" +
		"<attachment path=\"/tmp/a.txt\"/>\n" +
		"hello\n</message>"
	if header != want {
		t.Fatalf("header bytes changed:\n got: %q\nwant: %q", header, want)
	}
}

func TestFormatUserHeaderIncludesInteractionMetadata(t *testing.T) {
	t.Parallel()

	header := FormatUserHeader(UserMessageHeaderInput{
		MessageID:         "msg_1",
		ChannelIdentityID: "cid_1",
		DisplayName:       "Alice",
		Channel:           "feishu",
		ConversationType:  "group",
		ConversationName:  "Team Chat",
		Time:              time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
		Timezone:          "UTC",
		MentionsMe:        true,
		ReplyToMessageID:  "om_42",
		ReplyToSender:     `Bob "B" <ops>`,
		ForwardSender:     "Carol & Co",
	}, "hello")

	want := "<message id=\"msg_1\" sender=\"Alice\" t=\"2026-04-06T10:00:00Z\" channel=\"feishu\" conversation=\"Team Chat\" type=\"group\"" +
		" mentions_me=\"true\"" +
		" reply_to_message_id=\"om_42\"" +
		" reply_to_sender=\"Bob &quot;B&quot; &lt;ops&gt;\"" +
		" forwarded_from=\"Carol &amp; Co\">\n" +
		"hello\n</message>"
	if header != want {
		t.Fatalf("header mismatch:\n got: %q\nwant: %q", header, want)
	}
}

func TestFormatUserHeaderForwardedFromFallsBackToIDs(t *testing.T) {
	t.Parallel()

	base := UserMessageHeaderInput{
		DisplayName: "Alice",
		Channel:     "telegram",
		Time:        time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
	}

	byUser := base
	byUser.ForwardFromUserID = "u_9"
	if header := FormatUserHeader(byUser, "hi"); !strings.Contains(header, ` forwarded_from="user:u_9"`) {
		t.Fatalf("expected user fallback in header: %s", header)
	}

	byConversation := base
	byConversation.ForwardFromConversationID = "c_7"
	if header := FormatUserHeader(byConversation, "hi"); !strings.Contains(header, ` forwarded_from="conversation:c_7"`) {
		t.Fatalf("expected conversation fallback in header: %s", header)
	}
}

func TestFormatUserHeaderOmitsForwardedFromWhenAllOriginsEmpty(t *testing.T) {
	t.Parallel()

	header := FormatUserHeader(UserMessageHeaderInput{
		DisplayName:               "Alice",
		Channel:                   "telegram",
		Time:                      time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
		ForwardSender:             "  ",
		ForwardFromUserID:         " ",
		ForwardFromConversationID: "",
	}, "hi")

	if strings.Contains(header, "forwarded_from") {
		t.Fatalf("header must omit forwarded_from when every origin is empty: %s", header)
	}
}
