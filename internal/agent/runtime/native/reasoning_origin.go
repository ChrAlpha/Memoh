package native

import (
	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/partmeta"
)

// Reasoning is replayed to the model configuration that issued it and to no
// other. Providers bind opaque reasoning state (Anthropic signatures, OpenAI
// encrypted content and item ids, Gemini thought signatures) to the model,
// account and endpoint that produced it, and reject the whole request when it
// arrives anywhere else. Twilight can compare only the model name a response
// reported, which a relay rewrites and which two providers of one model
// share, so the step that commits reasoning records the Memoh model UUID it
// ran under (history attributes rows committed before that to the model the
// row records), and every provider call withholds reasoning recorded under a
// different one. Plain reasoning text is withheld the same way: it is the
// earlier model's inner monologue, and a model switch happens between turns,
// after the tool exchanges that needed it.

// stampReasoningOrigin records modelUUID as the issuer of each part. An empty
// modelUUID records nothing.
func stampReasoningOrigin(parts []sdk.ReasoningPart, modelUUID string) []sdk.ReasoningPart {
	if modelUUID == "" || len(parts) == 0 {
		return parts
	}
	stamped := make([]sdk.ReasoningPart, len(parts))
	for i, part := range parts {
		part.ProviderMetadata = partmeta.WithReasoningOrigin(part.ProviderMetadata, modelUUID)
		stamped[i] = part
	}
	return stamped
}

// isForeignReasoning reports whether part was issued under a model other than
// modelUUID. A part without a recorded origin cannot be shown to be foreign
// and is left to the provider's own replay checks.
func isForeignReasoning(part sdk.MessagePart, modelUUID string) bool {
	var reasoning sdk.ReasoningPart
	switch p := part.(type) {
	case sdk.ReasoningPart:
		reasoning = p
	case *sdk.ReasoningPart:
		if p == nil {
			return false
		}
		reasoning = *p
	default:
		return false
	}
	origin := partmeta.ReasoningOrigin(reasoning.ProviderMetadata)
	return origin != "" && origin != modelUUID
}

// withholdForeignReasoning returns messages without the reasoning other model
// configurations issued. An assistant message left with nothing to send is
// dropped, since an empty assistant turn is itself rejected. messages is not
// modified; it is returned as is when nothing is withheld.
func withholdForeignReasoning(messages []sdk.Message, modelUUID string) []sdk.Message {
	if modelUUID == "" {
		return messages
	}
	var out []sdk.Message
	for i, msg := range messages {
		if msg.Role != sdk.MessageRoleAssistant || !containsForeignReasoning(msg.Content, modelUUID) {
			if out != nil {
				out = append(out, msg)
			}
			continue
		}
		if out == nil {
			out = append(make([]sdk.Message, 0, len(messages)), messages[:i]...)
		}
		kept := make([]sdk.MessagePart, 0, len(msg.Content))
		for _, part := range msg.Content {
			if !isForeignReasoning(part, modelUUID) {
				kept = append(kept, part)
			}
		}
		if len(kept) == 0 {
			continue
		}
		msg.Content = kept
		out = append(out, msg)
	}
	if out == nil {
		return messages
	}
	return out
}

func containsForeignReasoning(parts []sdk.MessagePart, modelUUID string) bool {
	for _, part := range parts {
		if isForeignReasoning(part, modelUUID) {
			return true
		}
	}
	return false
}
