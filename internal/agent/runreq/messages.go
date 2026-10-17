package runreq

import (
	"encoding/json"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
	attachmentpkg "github.com/felinics/memoh/internal/attachment"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// SDKMessagesToModelMessages converts SDK messages to the persistence/API format
// for resolver call sites using the shared conversion helper. File parts are
// redacted to text placeholders first — document bytes never enter
// bot_history_messages (see redactFilePartsForStorage).
func SDKMessagesToModelMessages(msgs []sdk.Message) []ModelMessage {
	return historyfrag.ToStoredModelMessages(msgs)
}

// redactFilePartsForStorage replaces FileParts with text placeholders before
// persistence. Storing document base64 in message history would bloat rows,
// blow up the length-based token estimator (triggering history trimming), and
// deadlock replay after switching to a model without native file input. The
// file itself stays in the workspace: follow-up turns re-read it via the read
// tool, which re-injects it for whatever model is current.
func redactFilePartsForStorage(msgs []sdk.Message) []sdk.Message {
	return historyfrag.RedactFileParts(msgs)
}

// modelMessageToSDKMessage converts a persistence format message to SDK message
// at the resolver boundary using sdk.Message's native JSON deserialization.
func ModelMessageToSDKMessage(mm ModelMessage) sdk.Message {
	return historyfrag.StoredModelMessageToSDKMessage(mm)
}

// PrependTurnUserMessage puts the request's user message ahead of output.
func PrependTurnUserMessage(req ChatRequest, output []ModelMessage) []ModelMessage {
	if strings.TrimSpace(req.Query) == "" && req.UserMessageKind != UserMessageKindSkillActivation {
		return output
	}
	round := make([]ModelMessage, 0, 1+len(output))
	round = append(round, ModelMessage{
		Role:    "user",
		Content: NewTextContent(req.Query),
	})
	return append(round, output...)
}

// ModelQueryText is the query text the model sees for req.
func ModelQueryText(req ChatRequest) string {
	if strings.TrimSpace(req.ModelQuery) != "" {
		return req.ModelQuery
	}
	return req.Query
}

// ModelMessagesToSDKMessages converts a slice of persistence messages to SDK messages.
func ModelMessagesToSDKMessages(msgs []ModelMessage) []sdk.Message {
	return historyfrag.StoredModelMessagesToSDKMessages(msgs)
}

// Convert origins alongside each message so a rejected SDK message cannot
// shift the source marker onto a different user input.
func SDKMessagesWithOrigins(msgs []sdk.Message, indexes []int) []ModelMessage {
	feedback := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		feedback[index] = true
	}
	var result []ModelMessage
	for index, msg := range msgs {
		converted := SDKMessagesToModelMessages([]sdk.Message{msg})
		if feedback[index] && msg.Role == sdk.MessageRoleUser {
			for i := range converted {
				converted[i].Source = turnpkg.MessageSourceInternalFeedback
			}
		}
		result = append(result, converted...)
	}
	return result
}

// LastPersistedAssistantMessageID is the ID of the newest assistant row in messages.
func LastPersistedAssistantMessageID(messages []messagepkg.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "assistant") {
			return strings.TrimSpace(messages[i].ID)
		}
	}
	return ""
}

// IsEmptyAssistantMessage returns true if an assistant message has no
// meaningful content: no text, no tool calls, and no attachments.
func IsEmptyAssistantMessage(m ModelMessage) bool {
	if len(m.ToolCalls) > 0 {
		return false
	}
	text := strings.TrimSpace(m.TextContent())
	if text != "" {
		return false
	}
	var plain string
	if err := json.Unmarshal(m.Content, &plain); err == nil && strings.TrimSpace(plain) == "" {
		return true
	}
	// Check if content is empty array "[]" or null/empty
	content := strings.TrimSpace(string(m.Content))
	return content == "" || content == "[]" || content == "null"
}

// OutboundAssetRefsToMessageRefs converts outbound asset refs from the streaming
// collector into message-level asset refs for persistence.
func OutboundAssetRefsToMessageRefs(refs []OutboundAssetRef) []messagepkg.AssetRef {
	if len(refs) == 0 {
		return nil
	}
	result := make([]messagepkg.AssetRef, 0, len(refs))
	for _, ref := range refs {
		contentHash := strings.TrimSpace(ref.ContentHash)
		if contentHash == "" {
			continue
		}
		role := ref.Role
		if strings.TrimSpace(role) == "" {
			role = "attachment"
		}
		result = append(result, messagepkg.AssetRef{
			ContentHash: contentHash,
			Role:        role,
			Ordinal:     ref.Ordinal,
			Mime:        ref.Mime,
			SizeBytes:   ref.SizeBytes,
			StorageKey:  ref.StorageKey,
			Name:        ref.Name,
			Metadata:    ref.Metadata,
		})
	}
	return result
}

// ChatAttachmentsToAssetRefs converts ChatAttachment slice to message AssetRef slice.
// Only attachments that carry a content_hash are included.
func ChatAttachmentsToAssetRefs(attachments []ChatAttachment) []messagepkg.AssetRef {
	if len(attachments) == 0 {
		return nil
	}
	refs := make([]messagepkg.AssetRef, 0, len(attachments))
	for i, att := range attachments {
		contentHash := strings.TrimSpace(att.ContentHash)
		if contentHash == "" {
			continue
		}
		ref := messagepkg.AssetRef{
			ContentHash: contentHash,
			Role:        "attachment",
			Ordinal:     i,
			Mime:        strings.TrimSpace(att.Mime),
			SizeBytes:   att.Size,
			Name:        strings.TrimSpace(att.Name),
			Metadata:    att.Metadata,
		}
		ref.StorageKey = attachmentpkg.MetadataString(att.Metadata, attachmentpkg.MetadataKeyStorageKey)
		refs = append(refs, ref)
	}
	return refs
}
