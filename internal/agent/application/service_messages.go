package application

import (
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	turnpkg "github.com/felinics/memoh/internal/agent/turn"
)

// sdkMessagesToModelMessages converts SDK messages to the persistence/API format
// for resolver call sites using the shared conversion helper. File and image
// parts are redacted to text placeholders first — media bytes never enter
// bot_history_messages, and follow-up turns re-read the media via the read tool.
func sdkMessagesToModelMessages(msgs []sdk.Message) []ModelMessage {
	return historyfrag.ToStoredModelMessages(msgs)
}

// modelMessageToSDKMessage converts a persistence format message to SDK message
// at the resolver boundary using sdk.Message's native JSON deserialization.
func modelMessageToSDKMessage(mm ModelMessage) sdk.Message {
	return historyfrag.StoredModelMessageToSDKMessage(mm)
}

func prependTurnUserMessage(req ChatRequest, output []ModelMessage) []ModelMessage {
	if strings.TrimSpace(req.Query) == "" && req.UserMessageKind != UserMessageKindSkillActivation {
		return output
	}
	round := make([]ModelMessage, 0, 1+len(output))
	round = append(round, ModelMessage{
		Role:    "user",
		Content: newTextContent(req.Query),
	})
	return append(round, output...)
}

func modelQueryText(req ChatRequest) string {
	if strings.TrimSpace(req.ModelQuery) != "" {
		return req.ModelQuery
	}
	return req.Query
}

// modelMessagesToSDKMessages converts a slice of persistence messages to SDK messages.
func modelMessagesToSDKMessages(msgs []ModelMessage) []sdk.Message {
	return historyfrag.StoredModelMessagesToSDKMessages(msgs)
}

// Convert origins alongside each message so a rejected SDK message cannot
// shift the source marker onto a different user input.
func sdkMessagesWithOrigins(msgs []sdk.Message, indexes []int) []ModelMessage {
	feedback := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		feedback[index] = true
	}
	var result []ModelMessage
	for index, msg := range msgs {
		converted := sdkMessagesToModelMessages([]sdk.Message{msg})
		if feedback[index] && msg.Role == sdk.MessageRoleUser {
			for i := range converted {
				converted[i].Source = turnpkg.MessageSourceInternalFeedback
			}
		}
		result = append(result, converted...)
	}
	return result
}
