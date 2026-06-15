package matrix

import (
	"testing"

	"github.com/memohai/memoh/internal/channel"
)

func TestReadMatrixEditTarget_ReadsRelatesToEventID(t *testing.T) {
	t.Parallel()
	content := map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": "m.replace",
			"event_id": "$original",
		},
	}
	if got := readMatrixEditTarget(content); got != "$original" {
		t.Fatalf("expected $original, got %q", got)
	}
}

func TestReadMatrixEditTarget_NoRelatesToReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := readMatrixEditTarget(map[string]any{}); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestBuildMatrixEditedInbound_PopulatesEventTypeAndTarget(t *testing.T) {
	t.Parallel()
	evt := matrixEvent{
		Type:           "m.room.message",
		EventID:        "$edit",
		Sender:         "@alice:example.org",
		RoomID:         "!room:example.org",
		OriginServerTS: 1700000000000,
		Content: map[string]any{
			"msgtype": "m.text",
			"body":    "* updated body",
			"m.new_content": map[string]any{
				"msgtype": "m.text",
				"body":    "updated body",
			},
			"m.relates_to": map[string]any{
				"rel_type": "m.replace",
				"event_id": "$original",
			},
		},
	}
	msg, ok := buildMatrixEditedInbound(evt, "private")
	if !ok {
		t.Fatal("expected ok")
	}
	if got, _ := msg.Metadata["event_type"].(string); got != "edit" {
		t.Fatalf("expected event_type=edit, got %#v", msg.Metadata["event_type"])
	}
	if msg.Message.ID != "$original" {
		t.Fatalf("expected message ID to point at the edited original, got %q", msg.Message.ID)
	}
	if msg.Message.Text != "updated body" {
		t.Fatalf("expected new body, got %q", msg.Message.Text)
	}
	if got, _ := msg.Metadata["edit_target"].(string); got != "$original" {
		t.Fatalf("expected edit_target=$original, got %#v", msg.Metadata["edit_target"])
	}
	if got, _ := msg.Metadata["event_id"].(string); got != "$edit" {
		t.Fatalf("expected edit event_id preserved, got %#v", msg.Metadata["event_id"])
	}
	if msg.Channel != Type {
		t.Fatalf("expected matrix channel, got %q", msg.Channel)
	}
	if msg.Sender.SubjectID != "@alice:example.org" {
		t.Fatalf("expected sender preserved, got %q", msg.Sender.SubjectID)
	}
}

func TestBuildMatrixEditedInbound_RejectsEditWithoutTarget(t *testing.T) {
	t.Parallel()
	evt := matrixEvent{
		Type: "m.room.message",
		Content: map[string]any{
			"m.new_content": map[string]any{
				"msgtype": "m.text",
				"body":    "x",
			},
		},
	}
	if _, ok := buildMatrixEditedInbound(evt, "private"); ok {
		t.Fatal("expected false when relates_to is missing")
	}
}

func TestBuildMatrixEditedInbound_RejectsEditWithoutNewContent(t *testing.T) {
	t.Parallel()
	evt := matrixEvent{
		Type: "m.room.message",
		Content: map[string]any{
			"m.relates_to": map[string]any{
				"rel_type": "m.replace",
				"event_id": "$original",
			},
		},
	}
	if _, ok := buildMatrixEditedInbound(evt, "private"); ok {
		t.Fatal("expected false when m.new_content is missing")
	}
}

func TestBuildMatrixEditedInbound_RejectsEmptyContent(t *testing.T) {
	t.Parallel()
	evt := matrixEvent{
		Type: "m.room.message",
		Content: map[string]any{
			"m.new_content": map[string]any{
				"msgtype": "m.text",
				"body":    "",
			},
			"m.relates_to": map[string]any{
				"rel_type": "m.replace",
				"event_id": "$original",
			},
		},
	}
	if _, ok := buildMatrixEditedInbound(evt, "private"); ok {
		t.Fatal("expected false when new content body is empty and no attachments")
	}
}

func TestBuildMatrixEditedInbound_PreservesRoomMetadata(t *testing.T) {
	t.Parallel()
	evt := matrixEvent{
		Type:    "m.room.message",
		EventID: "$edit",
		Sender:  "@u:x",
		RoomID:  "!r:x",
		Content: map[string]any{
			"m.new_content": map[string]any{
				"msgtype": "m.text",
				"body":    "y",
			},
			"m.relates_to": map[string]any{
				"rel_type": "m.replace",
				"event_id": "$orig",
			},
		},
	}
	msg, ok := buildMatrixEditedInbound(evt, channel.ConversationTypeGroup)
	if !ok {
		t.Fatal("expected ok")
	}
	if msg.ReplyTarget != "!r:x" {
		t.Fatalf("expected reply target = room, got %q", msg.ReplyTarget)
	}
	if msg.Conversation.ID != "!r:x" {
		t.Fatalf("expected conversation id = room, got %q", msg.Conversation.ID)
	}
	if msg.Conversation.Type != channel.ConversationTypeGroup {
		t.Fatalf("expected group conversation, got %q", msg.Conversation.Type)
	}
}
