package historyfrag

import (
	"bytes"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/partmeta"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/messageconv"
)

// AttributeReasoningOrigin records modelUUID as the issuer of the reasoning in
// message that carries no origin of its own: reasoning committed before
// commits recorded it. The row records which model produced it, so its
// reasoning is withheld from a different model exactly like reasoning recorded
// at commit. Durable history keeps the row as stored, and the projected
// message keeps the stored version's ContextSource and the fields the SDK
// shape does not carry. It reports whether anything was attributed.
func AttributeReasoningOrigin(message turn.ModelMessage, modelUUID string) (turn.ModelMessage, bool) {
	if modelUUID == "" || !bytes.Contains(message.Content, []byte(`"reasoning"`)) {
		return message, false
	}
	sent := messageconv.ModelMessageToSDKMessage(message)
	attributed := false
	for i, part := range sent.Content {
		reasoning, ok := part.(sdk.ReasoningPart)
		if !ok || partmeta.ReasoningOrigin(reasoning.ProviderMetadata) != "" {
			continue
		}
		reasoning.ProviderMetadata = partmeta.WithReasoningOrigin(reasoning.ProviderMetadata, modelUUID)
		sent.Content[i] = reasoning
		attributed = true
	}
	if !attributed {
		return message, false
	}
	projected := messageconv.SDKMessagesToModelMessages([]sdk.Message{sent})[0]
	projected.Usage = message.Usage
	projected.ToolCalls = message.ToolCalls
	projected.ToolCallID = message.ToolCallID
	projected.Name = message.Name
	projected.ContextSource = message.ContextSource
	projected.Source = message.Source
	return projected, true
}
