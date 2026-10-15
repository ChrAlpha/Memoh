package turn

import (
	"strings"
	"time"
)

// UserMessageMeta holds the structured metadata attached to every user
// message. It is the single source of truth for the XML message tag sent to the LLM.
type UserMessageMeta struct {
	MessageID          string   `json:"message-id,omitempty"`
	ChannelIdentityID  string   `json:"channel-identity-id"`
	DisplayName        string   `json:"display-name"`
	Channel            string   `json:"channel"`
	ConversationType   string   `json:"conversation-type"`
	ConversationName   string   `json:"conversation-name,omitempty"`
	Target             string   `json:"target,omitempty"`
	Time               string   `json:"time"`
	Timezone           string   `json:"timezone,omitempty"`
	ForwardedFrom      string   `json:"forwarded-from,omitempty"`
	ForwardedMessageID string   `json:"forwarded-message-id,omitempty"`
	AttachmentPaths    []string `json:"attachments"`
	ReplyToMessageID   string   `json:"reply-to-message-id,omitempty"`
	ReplyToSender      string   `json:"reply-to-sender,omitempty"`
	ReplyToPreview     string   `json:"reply-to-preview,omitempty"`
	MentionsMe         bool     `json:"mentions-me,omitempty"`
	RepliesToMe        bool     `json:"replies-to-me,omitempty"`
}

// UserMessageHeaderInput is the unified input for building user message headers.
// Keeping this as a struct avoids long positional argument lists and makes
// future metadata extension backward-compatible for call sites.
type UserMessageHeaderInput struct {
	MessageID         string
	ChannelIdentityID string
	DisplayName       string
	Channel           string
	ConversationType  string
	ConversationName  string
	Target            string
	AttachmentPaths   []string
	Time              time.Time
	Timezone          string
	// Forward facts render as forwarded_from and forwarded_message_id, the same
	// attribute names the event timeline uses. Zero values add nothing.
	ForwardedFrom      string
	ForwardedMessageID string
	// Reply and mention facts are rendered the same way the event timeline
	// renders them, so a message looks identical whichever path persisted it.
	// Zero values add nothing to the header.
	ReplyToMessageID string
	ReplyToSender    string
	ReplyToPreview   string
	MentionsMe       bool
	RepliesToMe      bool
}

// BuildUserMessageMetaFromInput constructs metadata from one cohesive input.
func BuildUserMessageMetaFromInput(input UserMessageHeaderInput) UserMessageMeta {
	attachmentPaths := input.AttachmentPaths
	if attachmentPaths == nil {
		attachmentPaths = []string{}
	}
	meta := UserMessageMeta{
		MessageID:          input.MessageID,
		ChannelIdentityID:  input.ChannelIdentityID,
		DisplayName:        input.DisplayName,
		Channel:            input.Channel,
		ConversationType:   input.ConversationType,
		ConversationName:   input.ConversationName,
		Target:             strings.TrimSpace(input.Target),
		Time:               time.Now().UTC().Format(time.RFC3339),
		Timezone:           strings.TrimSpace(input.Timezone),
		ForwardedFrom:      strings.TrimSpace(input.ForwardedFrom),
		ForwardedMessageID: strings.TrimSpace(input.ForwardedMessageID),
		AttachmentPaths:    attachmentPaths,
		ReplyToMessageID:   strings.TrimSpace(input.ReplyToMessageID),
		ReplyToSender:      strings.TrimSpace(input.ReplyToSender),
		ReplyToPreview:     normalizeReplyPreview(input.ReplyToPreview),
		MentionsMe:         input.MentionsMe,
		RepliesToMe:        input.RepliesToMe,
	}
	if !input.Time.IsZero() {
		meta.Time = input.Time.Format(time.RFC3339)
	}
	return meta
}

// BuildUserMessageMetaWithTime constructs metadata with an explicit timestamp
// and timezone label for user-facing prompts.
func BuildUserMessageMetaWithTime(messageID, channelIdentityID, displayName, channel, conversationType, conversationName string, attachmentPaths []string, now time.Time, timezone string) UserMessageMeta {
	meta := BuildUserMessageMetaFromInput(UserMessageHeaderInput{
		MessageID:         messageID,
		ChannelIdentityID: channelIdentityID,
		DisplayName:       displayName,
		Channel:           channel,
		ConversationType:  conversationType,
		ConversationName:  conversationName,
		AttachmentPaths:   attachmentPaths,
		Time:              now,
		Timezone:          timezone,
	})
	if !now.IsZero() {
		meta.Time = now.Format(time.RFC3339)
	}
	meta.Timezone = strings.TrimSpace(timezone)
	return meta
}

// ToMap returns the metadata as a map with the same keys used in the XML
// attributes, suitable for storing as inbox content JSONB.
func (m UserMessageMeta) ToMap() map[string]any {
	result := map[string]any{
		"channel-identity-id": m.ChannelIdentityID,
		"display-name":        m.DisplayName,
		"channel":             m.Channel,
		"conversation-type":   m.ConversationType,
		"time":                m.Time,
		"attachments":         m.AttachmentPaths,
	}
	if m.MessageID != "" {
		result["message-id"] = m.MessageID
	}
	if m.ConversationName != "" {
		result["conversation-name"] = m.ConversationName
	}
	if m.Target != "" {
		result["target"] = m.Target
	}
	if strings.TrimSpace(m.Timezone) != "" {
		result["timezone"] = m.Timezone
	}
	if m.ForwardedFrom != "" {
		result["forwarded-from"] = m.ForwardedFrom
	}
	if m.ForwardedMessageID != "" {
		result["forwarded-message-id"] = m.ForwardedMessageID
	}
	if m.ReplyToMessageID != "" {
		result["reply-to-message-id"] = m.ReplyToMessageID
	}
	if m.ReplyToSender != "" {
		result["reply-to-sender"] = m.ReplyToSender
	}
	if m.ReplyToPreview != "" {
		result["reply-to-preview"] = m.ReplyToPreview
	}
	if m.MentionsMe {
		result["mentions-me"] = true
	}
	if m.RepliesToMe {
		result["replies-to-me"] = true
	}
	return result
}

// ForwardedFromHeader names where a forwarded message came from for the
// forwarded_from attribute: the forward's sender, else its user or conversation,
// else "unknown". Every writer of a user row renders a forward the same way.
func ForwardedFromHeader(sender, fromUserID, fromConversationID string) string {
	switch {
	case strings.TrimSpace(sender) != "":
		return strings.TrimSpace(sender)
	case strings.TrimSpace(fromUserID) != "":
		return "user:" + strings.TrimSpace(fromUserID)
	case strings.TrimSpace(fromConversationID) != "":
		return "conversation:" + strings.TrimSpace(fromConversationID)
	default:
		return "unknown"
	}
}

// FormatUserHeader wraps a user query in an XML <message> tag so the LLM sees
// structured context (sender, channel, conversation, time, attachments)
// alongside the raw message. This must be the single source of truth for
// user-message formatting — the agent gateway must NOT add its own header.
func FormatUserHeader(input UserMessageHeaderInput, query string) string {
	meta := BuildUserMessageMetaFromInput(input)
	return FormatUserHeaderFromMeta(meta, query)
}

// FormatUserHeaderFromMeta formats a pre-built UserMessageMeta into the
// XML <message> string sent to the LLM.
func FormatUserHeaderFromMeta(meta UserMessageMeta, query string) string {
	var sb strings.Builder

	sb.WriteString("<message")
	if meta.MessageID != "" {
		writeXMLAttr(&sb, "id", meta.MessageID)
	}
	writeXMLAttr(&sb, "sender", meta.DisplayName)
	if meta.MentionsMe {
		writeXMLAttr(&sb, "mentions_me", "true")
	}
	if meta.RepliesToMe {
		writeXMLAttr(&sb, "replies_to_me", "true")
	}
	writeXMLAttr(&sb, "t", meta.Time)
	writeXMLAttr(&sb, "channel", meta.Channel)
	if meta.ConversationName != "" {
		writeXMLAttr(&sb, "conversation", meta.ConversationName)
	}
	if meta.ConversationType != "" {
		writeXMLAttr(&sb, "type", meta.ConversationType)
	}
	if meta.Target != "" {
		writeXMLAttr(&sb, "target", meta.Target)
	}
	if meta.ForwardedFrom != "" {
		writeXMLAttr(&sb, "forwarded_from", meta.ForwardedFrom)
	}
	if meta.ForwardedMessageID != "" {
		writeXMLAttr(&sb, "forwarded_message_id", meta.ForwardedMessageID)
	}
	sb.WriteString(">\n")

	if meta.ReplyToMessageID != "" {
		sb.WriteString("<in-reply-to")
		writeXMLAttr(&sb, "id", meta.ReplyToMessageID)
		if meta.ReplyToSender != "" {
			writeXMLAttr(&sb, "sender", meta.ReplyToSender)
		}
		sb.WriteByte('>')
		sb.WriteString(escapeXMLText(meta.ReplyToPreview))
		sb.WriteString("</in-reply-to>\n")
	}

	if len(meta.AttachmentPaths) > 0 {
		for _, p := range meta.AttachmentPaths {
			sb.WriteString("<attachment path=\"")
			sb.WriteString(escapeXMLAttr(p))
			sb.WriteString("\"/>\n")
		}
	}

	sb.WriteString(query)
	sb.WriteString("\n</message>")
	return sb.String()
}

// UnwrapUserMessageEnvelope is the inverse of FormatUserHeaderFromMeta: it
// returns the text the user actually wrote, with the model-facing <message>
// wrapper and its structural children (<attachment>, <in-reply-to>) removed.
// Input that is not a complete envelope is returned unchanged, so callers can
// apply it defensively to any query string. An attachment-only message unwraps
// to the empty string — the envelope is the only content it ever had.
func UnwrapUserMessageEnvelope(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<message") {
		return text
	}

	openEnd := strings.IndexByte(trimmed, '>')
	if openEnd < 0 {
		return text
	}
	if strings.HasSuffix(strings.TrimSpace(trimmed[:openEnd+1]), "/>") {
		return ""
	}

	body := strings.TrimSpace(trimmed[openEnd+1:])
	if !strings.HasSuffix(body, "</message>") {
		return text
	}
	body = strings.TrimSpace(strings.TrimSuffix(body, "</message>"))

	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if isUserEnvelopeStructuralLine(strings.TrimSpace(line)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// isUserEnvelopeStructuralLine reports whether a line inside a <message>
// envelope is machine-generated context rather than user-written text.
func isUserEnvelopeStructuralLine(line string) bool {
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "<attachment ") && strings.HasSuffix(line, "/>") {
		return true
	}
	return strings.HasPrefix(line, "<in-reply-to ") && strings.HasSuffix(line, "</in-reply-to>")
}

// replyPreviewMaxRunes matches the event timeline's reply preview limit
// (chat/timeline replyPreviewMax), so a quoted message reads the same whether
// the row came from the timeline or from this header.
const replyPreviewMaxRunes = 100

// normalizeReplyPreview folds the preview onto one line, because
// UnwrapUserMessageEnvelope only strips an <in-reply-to> that fits on a single
// line, then truncates it the way the timeline does.
func normalizeReplyPreview(preview string) string {
	preview = strings.Join(strings.Fields(preview), " ")
	runes := []rune(preview)
	if len(runes) <= replyPreviewMaxRunes {
		return preview
	}
	return string(runes[:replyPreviewMaxRunes]) + "…"
}

// escapeXMLText escapes a string for use as XML element text.
func escapeXMLText(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

// escapeXMLAttr escapes a string for use inside an XML attribute value.
func escapeXMLAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
	)
	return r.Replace(s)
}

func writeXMLAttr(sb *strings.Builder, key, value string) {
	sb.WriteByte(' ')
	sb.WriteString(key)
	sb.WriteString("=\"")
	sb.WriteString(escapeXMLAttr(value))
	sb.WriteByte('"')
}
