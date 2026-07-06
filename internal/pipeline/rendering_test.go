package pipeline

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/channel"
)

func TestRenderMessage_ImageRefsPopulated(t *testing.T) {
	msg := &ICMessage{
		MessageID:    "msg-1",
		ReceivedAtMs: 100,
		TimestampSec: 100,
		Content:      []ContentNode{{Type: "text", Text: "photo"}},
		Attachments: []Attachment{
			{Type: "image", ContentHash: "hash-1", MimeType: "image/jpeg", FilePath: "/data/media/bot/ab/hash-1.jpg"},
			{Type: "file", ContentHash: "hash-2", MimeType: "application/pdf", FilePath: "/data/media/bot/cd/hash-2.pdf"},
			{Type: "image", MimeType: "image/png"},
		},
		Conversation: ConversationMeta{Channel: "telegram", ConversationType: "private"},
	}

	seg := renderMessage(msg, RenderParams{})

	if len(seg.ImageRefs) != 1 {
		t.Fatalf("expected 1 image ref (only images with ContentHash), got %d", len(seg.ImageRefs))
	}
	if seg.ImageRefs[0].ContentHash != "hash-1" {
		t.Fatalf("expected hash-1, got %q", seg.ImageRefs[0].ContentHash)
	}
	if seg.ImageRefs[0].Mime != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", seg.ImageRefs[0].Mime)
	}
}

func TestRenderMessage_NoImageRefs(t *testing.T) {
	msg := &ICMessage{
		MessageID:    "msg-2",
		ReceivedAtMs: 200,
		TimestampSec: 200,
		Content:      []ContentNode{{Type: "text", Text: "text only"}},
		Conversation: ConversationMeta{Channel: "telegram", ConversationType: "private"},
	}

	seg := renderMessage(msg, RenderParams{})

	if len(seg.ImageRefs) != 0 {
		t.Fatalf("expected 0 image refs, got %d", len(seg.ImageRefs))
	}
}

func TestRenderMessage_MetaAndICPopulated(t *testing.T) {
	msg := &ICMessage{
		MessageID:        "msg-7",
		Sender:           &CanonicalUser{ID: "u-1", DisplayName: "Alice", Username: "alice", IsBot: false},
		ReceivedAtMs:     1000,
		TimestampSec:     1,
		Content:          []ContentNode{{Type: "text", Text: "hello"}},
		ReplyToMessageID: "msg-3",
		ReplyToSender:    &CanonicalUser{ID: "u-2", DisplayName: "Bob"},
		ReplyToPreview:   "earlier",
		ForwardInfo: &ForwardInfo{
			MessageID:          "fwd-1",
			FromUserID:         "u-9",
			FromConversationID: "conv-9",
		},
		EditedAtSec:  2,
		Conversation: ConversationMeta{Channel: "telegram", ConversationType: "group"},
	}

	seg := renderMessage(msg, RenderParams{})

	if seg.Meta == nil {
		t.Fatal("expected Meta to be populated")
	}
	want := SegmentMeta{
		MessageID:                 "msg-7",
		SenderID:                  "u-1",
		SenderDisplayName:         "Alice",
		SenderUsername:            "alice",
		ReplyToMessageID:          "msg-3",
		ReplyToSender:             "Bob",
		ForwardMessageID:          "fwd-1",
		ForwardFromUserID:         "u-9",
		ForwardFromConversationID: "conv-9",
	}
	if *seg.Meta != want {
		t.Fatalf("Meta = %+v, want %+v", *seg.Meta, want)
	}
	if seg.IC == nil {
		t.Fatal("expected IC to be populated")
	}
	if seg.IC == msg {
		t.Fatal("expected IC to be a value copy, not the original pointer")
	}
	if seg.IC.MessageID != "msg-7" || seg.IC.ReplyToPreview != "earlier" {
		t.Fatalf("IC copy diverged: %+v", seg.IC)
	}
}

func TestRenderMessage_MetaReplyToSenderIsRaw(t *testing.T) {
	msg := &ICMessage{
		MessageID:        "msg-9",
		TimestampSec:     1,
		Content:          []ContentNode{{Type: "text", Text: "hi"}},
		ReplyToMessageID: "msg-3",
		ReplyToSender:    &CanonicalUser{ID: "u-2", DisplayName: "Bob", Username: "bob"},
		Conversation:     ConversationMeta{Channel: "telegram", ConversationType: "group"},
	}

	seg := renderMessage(msg, RenderParams{ContactNames: map[string]string{"u-2": "Contact Bob"}})

	if seg.Meta.ReplyToSender != "Bob" {
		t.Fatalf("Meta.ReplyToSender = %q, want the raw sender %q (decoration is a rendering concern)", seg.Meta.ReplyToSender, "Bob")
	}
	if !strings.Contains(seg.Content[0].Text, `sender="Contact Bob (@bob)"`) {
		t.Fatalf("rendered XML must keep the decorated reply sender label, got %s", seg.Content[0].Text)
	}
}

func TestRenderMessage_MetaReplyToSenderFallsBackToUsernameThenID(t *testing.T) {
	base := ICMessage{
		MessageID:        "msg-10",
		TimestampSec:     1,
		Content:          []ContentNode{{Type: "text", Text: "hi"}},
		ReplyToMessageID: "msg-3",
		Conversation:     ConversationMeta{Channel: "telegram"},
	}

	withUsername := base
	withUsername.ReplyToSender = &CanonicalUser{ID: "u-2", Username: "bob"}
	if got := renderMessage(&withUsername, RenderParams{}).Meta.ReplyToSender; got != "bob" {
		t.Fatalf("Meta.ReplyToSender = %q, want username fallback %q", got, "bob")
	}

	withIDOnly := base
	withIDOnly.ReplyToSender = &CanonicalUser{ID: "u-2"}
	if got := renderMessage(&withIDOnly, RenderParams{}).Meta.ReplyToSender; got != "u-2" {
		t.Fatalf("Meta.ReplyToSender = %q, want id fallback %q", got, "u-2")
	}
}

func TestRenderMessage_MetaOnDeletedMessage(t *testing.T) {
	msg := &ICMessage{
		MessageID:    "msg-8",
		Sender:       &CanonicalUser{ID: "u-1", DisplayName: "Alice"},
		ReceivedAtMs: 1000,
		TimestampSec: 1,
		Deleted:      true,
		Conversation: ConversationMeta{Channel: "telegram"},
	}

	seg := renderMessage(msg, RenderParams{})

	if seg.Meta == nil || seg.Meta.MessageID != "msg-8" {
		t.Fatalf("Meta = %+v, want MessageID msg-8 on deleted message segment", seg.Meta)
	}
	if seg.IC == nil {
		t.Fatal("expected IC on deleted message segment")
	}
}

func TestRender_SystemEventSegmentHasNoMeta(t *testing.T) {
	ic := NewEmptyIC("s1")
	ic.Nodes = []ICNode{{SystemEvent: &ICSystemEvent{
		Type:         "system_event",
		Kind:         "chat_photo_changed",
		ReceivedAtMs: 100,
		TimestampSec: 1,
	}}}

	rc := Render(ic, RenderParams{})

	if len(rc) != 1 {
		t.Fatalf("segments = %d, want 1", len(rc))
	}
	if rc[0].Meta != nil || rc[0].IC != nil {
		t.Fatalf("system event segment must not carry Meta/IC, got Meta=%+v IC=%+v", rc[0].Meta, rc[0].IC)
	}
}

func TestAdaptAttachments_ContentHash(t *testing.T) {
	atts := []channel.Attachment{
		{Type: channel.AttachmentImage, ContentHash: "abc123", URL: "/data/media/bot/ab/abc123.jpg", Mime: "image/jpeg"},
		{Type: channel.AttachmentFile, URL: "https://example.com/doc.pdf", Mime: "application/pdf"},
	}
	got := adaptAttachments(atts)
	if len(got) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(got))
	}
	if got[0].ContentHash != "abc123" || got[0].MimeType != "image/jpeg" {
		t.Fatalf("unexpected first attachment: %+v", got[0])
	}
	if got[0].FilePath != "/data/media/bot/ab/abc123.jpg" {
		t.Fatalf("expected FilePath from URL, got %q", got[0].FilePath)
	}
	if got[1].Type != "file" || got[1].MimeType != "application/pdf" {
		t.Fatalf("unexpected second attachment: %+v", got[1])
	}
	if got[1].FilePath != "https://example.com/doc.pdf" {
		t.Fatalf("expected FilePath from URL, got %q", got[1].FilePath)
	}
}
