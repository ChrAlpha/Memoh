package historyfrag

import (
	"reflect"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/partmeta"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/messageconv"
)

func storedAssistant(t *testing.T, parts ...sdk.MessagePart) messagepkg.Message {
	t.Helper()
	modelMessage := messageconv.SDKMessagesToModelMessages([]sdk.Message{{Role: sdk.MessageRoleAssistant, Content: parts}})[0]
	return messagepkg.Message{
		ID:        "row-1",
		BotID:     "bot-1",
		SessionID: "sess-1",
		Role:      "assistant",
		Content:   persistedModelMessage(t, modelMessage),
		ModelID:   "model-a",
		CreatedAt: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
	}
}

func reasoningParts(message turn.ModelMessage) []sdk.ReasoningPart {
	var parts []sdk.ReasoningPart
	for _, part := range messageconv.ModelMessageToSDKMessage(message).Content {
		if p, ok := part.(sdk.ReasoningPart); ok {
			parts = append(parts, p)
		}
	}
	return parts
}

// A row committed before reasoning recorded its origin is attributed to the
// model the row records, while its replay identity stays the stored version.
func TestAttributeReasoningOriginAttributesUnrecordedReasoning(t *testing.T) {
	t.Parallel()

	row := storedAssistant(t,
		sdk.ReasoningPart{
			ID: "0f8e2c1a", Text: "relay thought", Format: sdk.ReasoningFormatOpenAIResponses,
			ProviderMetadata: sdk.ProviderMetadata{"openai": {"itemId": "0f8e2c1a", "reasoningEncryptedContent": "RELAY"}},
		},
		sdk.TextPart{Text: "answer"},
		sdk.ToolCallPart{ToolCallID: "call-1", ToolName: "read", Input: sdk.ParseToolArguments(`{"path":"a"}`)},
	)
	record, err := FromDBMessage(row, ScopeFallback{})
	if err != nil {
		t.Fatalf("FromDBMessage: %v", err)
	}
	if record.ModelID != "model-a" {
		t.Fatalf("record model = %q, want the row's model", record.ModelID)
	}
	storedHash := record.ModelMessage.ContextSource.ContentHash

	attributed, ok := AttributeReasoningOrigin(record.ModelMessage, record.ModelID)
	if !ok {
		t.Fatal("unrecorded reasoning was not attributed")
	}
	parts := reasoningParts(attributed)
	if len(parts) != 1 || partmeta.ReasoningOrigin(parts[0].ProviderMetadata) != "model-a" {
		t.Fatalf("reasoning = %#v, want it attributed to model-a", parts)
	}
	if parts[0].ProviderMetadata.Get("openai", "reasoningEncryptedContent") != "RELAY" {
		t.Fatalf("provider token lost: %#v", parts[0].ProviderMetadata)
	}
	if attributed.ContextSource == nil || attributed.ContextSource.ContentHash != storedHash {
		t.Fatalf("context source = %#v, want the stored version's hash %q", attributed.ContextSource, storedHash)
	}
	sent := messageconv.ModelMessageToSDKMessage(attributed)
	if len(sent.Content) != 3 {
		t.Fatalf("content = %#v, want reasoning, text and the tool call", sent.Content)
	}
	if call, ok := sent.Content[2].(sdk.ToolCallPart); !ok || call.ToolCallID != "call-1" {
		t.Fatalf("tool call = %#v", sent.Content[2])
	}
	if len(reasoningParts(record.ModelMessage)) != 1 || partmeta.ReasoningOrigin(reasoningParts(record.ModelMessage)[0].ProviderMetadata) != "" {
		t.Fatal("the record's own message was modified")
	}
}

func TestAttributeReasoningOriginLeavesOtherMessagesAlone(t *testing.T) {
	t.Parallel()

	recorded := storedAssistant(t,
		sdk.ReasoningPart{
			Text: "recorded", Format: sdk.ReasoningFormatAnthropic,
			ProviderMetadata: partmeta.WithReasoningOrigin(sdk.ProviderMetadata{"anthropic": {"signature": "SIG"}}, "model-b"),
		},
		sdk.TextPart{Text: "answer"},
	)
	plain := storedAssistant(t, sdk.TextPart{Text: "no reasoning here"})
	for name, tc := range map[string]struct {
		row     messagepkg.Message
		modelID string
	}{
		"recorded origin": {recorded, "model-a"},
		"no reasoning":    {plain, "model-a"},
		"unknown model":   {storedAssistant(t, sdk.ReasoningPart{Text: "thought"}), ""},
	} {
		t.Run(name, func(t *testing.T) {
			record, err := FromDBMessage(tc.row, ScopeFallback{})
			if err != nil {
				t.Fatalf("FromDBMessage: %v", err)
			}
			got, ok := AttributeReasoningOrigin(record.ModelMessage, tc.modelID)
			if ok || !reflect.DeepEqual(got, record.ModelMessage) {
				t.Fatalf("message was rewritten: ok=%v\n got  %#v\n want %#v", ok, got, record.ModelMessage)
			}
		})
	}
	record, err := FromDBMessage(recorded, ScopeFallback{})
	if err != nil {
		t.Fatalf("FromDBMessage: %v", err)
	}
	if parts := reasoningParts(record.ModelMessage); len(parts) != 1 || partmeta.ReasoningOrigin(parts[0].ProviderMetadata) != "model-b" {
		t.Fatalf("recorded origin = %#v, want model-b kept", parts)
	}
}
