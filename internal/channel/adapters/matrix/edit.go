package matrix

import (
	"strings"

	"github.com/memohai/memoh/internal/channel"
)

func readMatrixEditTarget(content map[string]any) string {
	relatesTo, ok := content["m.relates_to"].(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(channel.ReadString(relatesTo, "event_id"))
}

func buildMatrixEditedInbound(evt matrixEvent, conversationType string) (channel.InboundMessage, bool) {
	target := readMatrixEditTarget(evt.Content)
	if target == "" {
		return channel.InboundMessage{}, false
	}
	newContent, ok := evt.Content["m.new_content"].(map[string]any)
	if !ok {
		return channel.InboundMessage{}, false
	}
	body, attachments := extractMatrixInboundContent(newContent)
	if body == "" && len(attachments) == 0 {
		return channel.InboundMessage{}, false
	}
	return channel.InboundMessage{
		Channel:     Type,
		ReplyTarget: strings.TrimSpace(evt.RoomID),
		Message: channel.Message{
			ID:          target,
			Format:      channel.MessageFormatPlain,
			Text:        body,
			Attachments: attachments,
		},
		Sender: channel.Identity{
			SubjectID:   strings.TrimSpace(evt.Sender),
			DisplayName: matrixDisplayName(evt),
			Attributes: map[string]string{
				"user_id": strings.TrimSpace(evt.Sender),
				"room_id": strings.TrimSpace(evt.RoomID),
			},
		},
		Conversation: channel.Conversation{
			ID:   strings.TrimSpace(evt.RoomID),
			Type: conversationType,
			Metadata: map[string]any{
				"room_id": strings.TrimSpace(evt.RoomID),
			},
		},
		ReceivedAt: matrixEventTime(evt.OriginServerTS),
		Source:     "matrix",
		Metadata: map[string]any{
			"event_type":  "edit",
			"room_id":     strings.TrimSpace(evt.RoomID),
			"event_id":    strings.TrimSpace(evt.EventID),
			"edit_target": target,
			"sender":      strings.TrimSpace(evt.Sender),
		},
	}, true
}
