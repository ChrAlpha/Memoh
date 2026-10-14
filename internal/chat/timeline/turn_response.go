package timeline

import (
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/messageconv"
)

// ProjectInterruptedReasoning exposes an unfinished reasoning part as text so
// every provider can continue it on the next turn. Other message fields and
// content parts remain unchanged.
func ProjectInterruptedReasoning(modelMsg turn.ModelMessage) turn.ModelMessage {
	message := messageconv.ModelMessageToSDKMessage(modelMsg)
	var reasoning strings.Builder
	parts := make([]sdk.MessagePart, 0, len(message.Content))
	for _, part := range message.Content {
		switch p := part.(type) {
		case sdk.ReasoningPart:
			reasoning.WriteString(p.Text)
		case *sdk.ReasoningPart:
			reasoning.WriteString(p.Text)
		default:
			parts = append(parts, part)
		}
	}
	if strings.TrimSpace(reasoning.String()) == "" {
		return modelMsg
	}

	checkpoint := messagepkg.AgentStepInterruptedReasoningPrefix + reasoning.String()
	for i, part := range parts {
		switch p := part.(type) {
		case sdk.TextPart:
			p.Text = checkpoint + "\n\n" + p.Text
			parts[i], checkpoint = p, ""
		case *sdk.TextPart:
			clone := *p
			clone.Text = checkpoint + "\n\n" + clone.Text
			parts[i], checkpoint = clone, ""
		}
		if checkpoint == "" {
			break
		}
	}
	if checkpoint != "" {
		parts = append([]sdk.MessagePart{sdk.TextPart{Text: checkpoint}}, parts...)
	}
	message.Content = parts
	projected := messageconv.SDKMessagesToModelMessages([]sdk.Message{message})[0]
	projected.Usage = modelMsg.Usage
	projected.ToolCalls = modelMsg.ToolCalls
	projected.ToolCallID = modelMsg.ToolCallID
	projected.Name = modelMsg.Name
	return projected
}
