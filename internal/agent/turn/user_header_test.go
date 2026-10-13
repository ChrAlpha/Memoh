package turn

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

func TestUnwrapUserMessageEnvelope(t *testing.T) {
	t.Parallel()

	input := UserMessageHeaderInput{
		DisplayName:      "User",
		Channel:          "web",
		ConversationType: "private",
		Target:           "115e7013-dc2a-4437-8e21-b49fbb21dfef",
		AttachmentPaths:  []string{"/data/.memoh/media/b2/b2edf40e.png"},
		Time:             time.Date(2026, 8, 20, 17, 37, 14, 0, time.UTC),
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{"attachment only", FormatUserHeader(input, ""), ""},
		{"attachment with caption", FormatUserHeader(input, "look at this"), "look at this"},
		{"self closing", `<message sender="User" t="now"/>`, ""},
		{"plain text passthrough", "hello world", "hello world"},
		{"unterminated envelope passthrough", `<message sender="User">hello`, `<message sender="User">hello`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := UnwrapUserMessageEnvelope(tc.text); got != tc.want {
				t.Fatalf("UnwrapUserMessageEnvelope(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestFormatUserHeaderZeroReplyAndMentionIsByteIdentical(t *testing.T) {
	t.Parallel()

	input := UserMessageHeaderInput{
		MessageID:        "msg_1",
		DisplayName:      "Alice",
		Channel:          "feishu",
		ConversationType: "group",
		AttachmentPaths:  []string{"/tmp/a.txt"},
		Time:             time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
	}
	want := "<message id=\"msg_1\" sender=\"Alice\" t=\"2026-04-06T10:00:00Z\" channel=\"feishu\" type=\"group\">\n" +
		"<attachment path=\"/tmp/a.txt\"/>\nhello\n</message>"
	if got := FormatUserHeader(input, "hello"); got != want {
		t.Fatalf("header changed for zero reply/mention input\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFormatUserHeaderRendersReplyAndMention(t *testing.T) {
	t.Parallel()

	input := UserMessageHeaderInput{
		MessageID:        "msg_2",
		DisplayName:      "Alice",
		Channel:          "feishu",
		ConversationType: "group",
		Time:             time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
		ReplyToMessageID: "msg_1",
		ReplyToSender:    "Bob",
		MentionsMe:       true,
		RepliesToMe:      true,
	}
	want := "<message id=\"msg_2\" sender=\"Alice\" mentions_me=\"true\" replies_to_me=\"true\" t=\"2026-04-06T10:00:00Z\" channel=\"feishu\" type=\"group\">\n" +
		"<in-reply-to id=\"msg_1\" sender=\"Bob\"></in-reply-to>\nhello\n</message>"
	got := FormatUserHeader(input, "hello")
	if got != want {
		t.Fatalf("header\ngot:  %q\nwant: %q", got, want)
	}
	if unwrapped := UnwrapUserMessageEnvelope(got); unwrapped != "hello" {
		t.Fatalf("unwrap = %q, want hello", unwrapped)
	}
}

func TestFormatUserHeaderRendersReplyPreview(t *testing.T) {
	t.Parallel()

	input := UserMessageHeaderInput{
		MessageID:        "msg_2",
		DisplayName:      "Alice",
		Channel:          "telegram",
		Time:             time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
		ReplyToMessageID: "msg_1",
		ReplyToSender:    "Bob",
		ReplyToPreview:   "  is a < b && c > d?\n\tyes  ",
	}
	want := "<message id=\"msg_2\" sender=\"Alice\" t=\"2026-04-06T10:00:00Z\" channel=\"telegram\">\n" +
		"<in-reply-to id=\"msg_1\" sender=\"Bob\">is a &lt; b &amp;&amp; c &gt; d? yes</in-reply-to>\nhello\n</message>"
	got := FormatUserHeader(input, "hello")
	if got != want {
		t.Fatalf("header\ngot:  %q\nwant: %q", got, want)
	}
	if unwrapped := UnwrapUserMessageEnvelope(got); unwrapped != "hello" {
		t.Fatalf("unwrap = %q, want hello", unwrapped)
	}
}

func TestFormatUserHeaderTruncatesLongReplyPreview(t *testing.T) {
	t.Parallel()

	// Truncation counts runes of the raw text, before escaping, so an entity
	// is never cut in half.
	preview := strings.Repeat("界", replyPreviewMaxRunes-1) + "<" + "tail"
	input := UserMessageHeaderInput{
		DisplayName:      "Alice",
		Channel:          "telegram",
		Time:             time.Date(2026, 4, 6, 10, 0, 0, 0, time.UTC),
		ReplyToMessageID: "msg_1",
		ReplyToPreview:   preview,
	}
	wantBody := strings.Repeat("界", replyPreviewMaxRunes-1) + "&lt;…"
	got := FormatUserHeader(input, "hello")
	if !strings.Contains(got, "<in-reply-to id=\"msg_1\">"+wantBody+"</in-reply-to>\n") {
		t.Fatalf("preview not truncated to %d runes: %q", replyPreviewMaxRunes, got)
	}
	if strings.Contains(got, "tail") {
		t.Fatalf("text past the limit leaked: %q", got)
	}
}
