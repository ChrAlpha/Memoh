package timeline

import "testing"

// These golden tests lock the exact XML bytes renderMessage and
// renderSystemEventXML produce today. These bytes are what the model sees,
// and RenderMessageSegment promises byte-identical re-rendering, so any
// drift here would silently change both. Do not update the expected
// strings to make a refactor pass; make the refactor byte-preserving.

func TestRenderMessageGoldenBytes(t *testing.T) {
	cases := []struct {
		name   string
		msg    *ICMessage
		params RenderParams
		want   string
	}{
		{
			name: "plain text with full conversation meta",
			msg: &ICMessage{
				MessageID:    "m1",
				Sender:       &CanonicalUser{ID: "u1", DisplayName: "Alice", Username: "alice"},
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "hello world"}},
				Conversation: ConversationMeta{Channel: "telegram", ConversationName: "Dev Room", ConversationType: "group", Target: "chat-42"},
			},
			want: `<message id="m1" sender="Alice (@alice)" t="1970-01-01T00:00:00+00:00" channel="telegram" conversation="Dev Room" type="group" target="chat-42">` + "\n" +
				"hello world\n</message>",
		},
		{
			name: "rich content nodes mention link code pre bold",
			msg: &ICMessage{
				MessageID:    "m2",
				TimestampSec: 0,
				Content: []ContentNode{
					{Type: "mention", UserID: "u2", Children: []ContentNode{{Type: "text", Text: "Bob"}}},
					{Type: "text", Text: " check "},
					{Type: "link", URL: "https://ex.com/?a=1&b=2", Children: []ContentNode{{Type: "text", Text: "here"}}},
					{Type: "text", Text: " "},
					{Type: "code", Text: "x < 1"},
					{Type: "pre", Language: "go", Text: "a & b"},
					{Type: "bold", Children: []ContentNode{{Type: "text", Text: "hot"}}},
				},
				Conversation: ConversationMeta{Channel: "discord"},
			},
			want: `<message id="m2" t="1970-01-01T00:00:00+00:00" channel="discord">` + "\n" +
				`<mention uid="u2">Bob</mention> check <a href="https://ex.com/?a=1&amp;b=2">here</a> <code>x &lt; 1</code><pre lang="go">a &amp; b</pre><b>hot</b>` + "\n</message>",
		},
		{
			name: "reply with escaped preview",
			msg: &ICMessage{
				MessageID:        "m3",
				TimestampSec:     0,
				Content:          []ContentNode{{Type: "text", Text: "sure"}},
				ReplyToMessageID: "m0",
				ReplyToSender:    &CanonicalUser{DisplayName: "Bob"},
				ReplyToPreview:   `he said "hi" & <bye>`,
				Conversation:     ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m3" t="1970-01-01T00:00:00+00:00" channel="telegram">` + "\n" +
				`<in-reply-to id="m0" sender="Bob">he said "hi" &amp; &lt;bye&gt;</in-reply-to>` + "\n" +
				"sure\n</message>",
		},
		{
			name: "forward with sender name and message id",
			msg: &ICMessage{
				MessageID:    "m4",
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "fwd body"}},
				ForwardInfo:  &ForwardInfo{MessageID: "f1", SenderName: "Carol"},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m4" t="1970-01-01T00:00:00+00:00" channel="telegram" forwarded_from="Carol" forwarded_message_id="f1">` + "\n" +
				"fwd body\n</message>",
		},
		{
			name: "forward with user id origin only",
			msg: &ICMessage{
				MessageID:    "m5",
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "fwd body"}},
				ForwardInfo:  &ForwardInfo{FromUserID: "u9"},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m5" t="1970-01-01T00:00:00+00:00" channel="telegram" forwarded_from="user:u9">` + "\n" +
				"fwd body\n</message>",
		},
		{
			name: "attachments image alt file and audio duration",
			msg: &ICMessage{
				MessageID:    "m6",
				TimestampSec: 0,
				Attachments: []Attachment{
					{Type: "image", MimeType: "image/png", FileName: "cat.png", Width: 10, Height: 20, FilePath: "/tmp/cat.png", AltText: "a <cat>"},
					{Type: "file", MimeType: "application/pdf", FileName: "doc.pdf", FilePath: "/tmp/doc.pdf"},
					{Type: "audio", MimeType: "audio/ogg", Duration: 5},
				},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m6" t="1970-01-01T00:00:00+00:00" channel="telegram">` + "\n" +
				`<image type="image" mime="image/png" name="cat.png" size="10x20" path="/tmp/cat.png">a &lt;cat&gt;</image>` + "\n" +
				`<attachment type="file" mime="application/pdf" name="doc.pdf" path="/tmp/doc.pdf"/>` + "\n" +
				`<attachment type="audio" mime="audio/ogg" duration="5"/>` + "\n</message>",
		},
		{
			name: "deleted message self-closing",
			msg: &ICMessage{
				MessageID:    "m7",
				Sender:       &CanonicalUser{ID: "u1", DisplayName: "Alice"},
				TimestampSec: 0,
				Deleted:      true,
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m7" sender="Alice" t="1970-01-01T00:00:00+00:00" channel="telegram"/>`,
		},
		{
			name: "edited timestamp attribute",
			msg: &ICMessage{
				MessageID:    "m8",
				TimestampSec: 0,
				EditedAtSec:  60,
				Content:      []ContentNode{{Type: "text", Text: "fixed"}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m8" t="1970-01-01T00:00:00+00:00" edited="1970-01-01T00:01:00+00:00" channel="telegram">` + "\n" +
				"fixed\n</message>",
		},
		{
			name: "attribute escaping includes quotes text escaping does not",
			msg: &ICMessage{
				MessageID:    "m9",
				Sender:       &CanonicalUser{ID: "u1", DisplayName: `A"B & <C>`},
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: `1 < 2 & "quotes" stay`}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m9" sender="A&quot;B &amp; &lt;C&gt;" t="1970-01-01T00:00:00+00:00" channel="telegram">` + "\n" +
				`1 &lt; 2 &amp; "quotes" stay` + "\n</message>",
		},
		{
			name: "positive utc offset pads hours and minutes",
			msg: &ICMessage{
				MessageID:    "m10",
				TimestampSec: 0,
				UTCOffsetMin: 330,
				Content:      []ContentNode{{Type: "text", Text: "tz"}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m10" t="1970-01-01T05:30:00+05:30" channel="telegram">` + "\ntz\n</message>",
		},
		{
			name: "negative utc offset pads hours and minutes",
			msg: &ICMessage{
				MessageID:    "m11",
				TimestampSec: 0,
				UTCOffsetMin: -225,
				Content:      []ContentNode{{Type: "text", Text: "tz"}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m11" t="1969-12-31T20:15:00-03:45" channel="telegram">` + "\ntz\n</message>",
		},
		{
			name: "contact name overrides sender display name",
			msg: &ICMessage{
				MessageID:    "m12",
				Sender:       &CanonicalUser{ID: "u1", DisplayName: "Alice", Username: "alice"},
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "hi"}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			params: RenderParams{ContactNames: map[string]string{"u1": "Contact Alice"}},
			want: `<message id="m12" sender="Contact Alice (@alice)" t="1970-01-01T00:00:00+00:00" channel="telegram">` + "\n" +
				"hi\n</message>",
		},
		{
			name: "myself attribute when bot user id matches sender",
			msg: &ICMessage{
				MessageID:    "m13",
				Sender:       &CanonicalUser{ID: "u1", DisplayName: "Alice", Username: "alice"},
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "hi"}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			params: RenderParams{BotUserID: "u1"},
			want: `<message id="m13" sender="Alice (@alice)" myself="true" t="1970-01-01T00:00:00+00:00" channel="telegram">` + "\n" +
				"hi\n</message>",
		},
		{
			name: "forward with canonical sender is decorated",
			msg: &ICMessage{
				MessageID:    "m14",
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "fwd body"}},
				ForwardInfo:  &ForwardInfo{MessageID: "f2", Sender: &CanonicalUser{ID: "u7", DisplayName: "Dana", Username: "dana"}},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m14" t="1970-01-01T00:00:00+00:00" channel="telegram" forwarded_from="Dana (@dana)" forwarded_message_id="f2">` + "\n" +
				"fwd body\n</message>",
		},
		{
			name: "forward with conversation origin only",
			msg: &ICMessage{
				MessageID:    "m15",
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "fwd body"}},
				ForwardInfo:  &ForwardInfo{FromConversationID: "c7"},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m15" t="1970-01-01T00:00:00+00:00" channel="telegram" forwarded_from="conversation:c7">` + "\n" +
				"fwd body\n</message>",
		},
		{
			name: "forward without origin is unknown",
			msg: &ICMessage{
				MessageID:    "m16",
				TimestampSec: 0,
				Content:      []ContentNode{{Type: "text", Text: "fwd body"}},
				ForwardInfo:  &ForwardInfo{},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m16" t="1970-01-01T00:00:00+00:00" channel="telegram" forwarded_from="unknown">` + "\n" +
				"fwd body\n</message>",
		},
		{
			name: "rich content nodes italic underline strikethrough spoiler blockquote",
			msg: &ICMessage{
				MessageID:    "m17",
				TimestampSec: 0,
				Content: []ContentNode{
					{Type: "italic", Children: []ContentNode{{Type: "text", Text: "it"}}},
					{Type: "underline", Children: []ContentNode{{Type: "text", Text: "ul"}}},
					{Type: "strikethrough", Children: []ContentNode{{Type: "text", Text: "st"}}},
					{Type: "spoiler", Children: []ContentNode{{Type: "text", Text: "sp"}}},
					{Type: "blockquote", Children: []ContentNode{{Type: "text", Text: "bq"}}},
				},
				Conversation: ConversationMeta{Channel: "telegram"},
			},
			want: `<message id="m17" t="1970-01-01T00:00:00+00:00" channel="telegram">` + "\n" +
				`<i>it</i><u>ul</u><s>st</s><spoiler>sp</spoiler><blockquote>bq</blockquote>` + "\n</message>",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seg := renderMessage(tc.msg, tc.params)
			if len(seg.Content) != 1 || seg.Content[0].Type != "text" {
				t.Fatalf("content = %#v, want single text piece", seg.Content)
			}
			if got := seg.Content[0].Text; got != tc.want {
				t.Fatalf("rendered bytes drifted:\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}

func TestRenderSystemEventGoldenBytes(t *testing.T) {
	admin := &CanonicalUser{ID: "u0", DisplayName: "Admin"}
	cases := []struct {
		name  string
		event *ICSystemEvent
		want  string
	}{
		{
			name: "user renamed",
			event: &ICSystemEvent{
				Kind:    "user_renamed",
				OldUser: &CanonicalUser{DisplayName: "Old"},
				NewUser: &CanonicalUser{DisplayName: "New"},
			},
			want: `<event type="name_change" t="1970-01-01T00:00:00+00:00" from_name="Old" to_name="New"/>`,
		},
		{
			name: "members joined with actor",
			event: &ICSystemEvent{
				Kind:    "members_joined",
				Actor:   admin,
				Members: []CanonicalUser{{DisplayName: "Bob"}, {DisplayName: "Carol"}},
			},
			want: `<event type="members_joined" t="1970-01-01T00:00:00+00:00" actor="Admin" members="Bob, Carol"/>`,
		},
		{
			name: "member left without actor",
			event: &ICSystemEvent{
				Kind:   "member_left",
				Member: &CanonicalUser{DisplayName: "Bob"},
			},
			want: `<event type="member_left" t="1970-01-01T00:00:00+00:00" member="Bob"/>`,
		},
		{
			name: "chat renamed with previous title",
			event: &ICSystemEvent{
				Kind:     "chat_renamed",
				Actor:    admin,
				OldTitle: "Old Title",
				NewTitle: "New Title",
			},
			want: `<event type="chat_renamed" t="1970-01-01T00:00:00+00:00" actor="Admin" from="Old Title" to="New Title"/>`,
		},
		{
			name:  "chat photo changed",
			event: &ICSystemEvent{Kind: "chat_photo_changed", Actor: admin},
			want:  `<event type="chat_photo_changed" t="1970-01-01T00:00:00+00:00" actor="Admin"/>`,
		},
		{
			name:  "chat photo deleted",
			event: &ICSystemEvent{Kind: "chat_photo_deleted"},
			want:  `<event type="chat_photo_deleted" t="1970-01-01T00:00:00+00:00"/>`,
		},
		{
			name: "message pinned with preview",
			event: &ICSystemEvent{
				Kind:            "message_pinned",
				PinnedMessageID: "m0",
				PinnedPreview:   "pin <text>",
			},
			want: `<event type="message_pinned" t="1970-01-01T00:00:00+00:00" message_id="m0">pin &lt;text&gt;</event>`,
		},
		{
			name: "message pinned without preview",
			event: &ICSystemEvent{
				Kind:            "message_pinned",
				PinnedMessageID: "m0",
			},
			want: `<event type="message_pinned" t="1970-01-01T00:00:00+00:00" message_id="m0"/>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderSystemEventXML(tc.event, nil); got != tc.want {
				t.Fatalf("rendered bytes drifted:\n--- got ---\n%s\n--- want ---\n%s", got, tc.want)
			}
		})
	}
}
