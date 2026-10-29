package application

import (
	"context"
	"log/slog"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	historyfrag "github.com/felinics/memoh/internal/agent/context/history"
	"github.com/felinics/memoh/internal/agent/partmeta"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func historyReasoningOrigins(t *testing.T, message ModelMessage) []string {
	t.Helper()
	var origins []string
	for _, part := range modelMessageToSDKMessage(message).Content {
		if p, ok := part.(sdk.ReasoningPart); ok {
			origins = append(origins, partmeta.ReasoningOrigin(p.ProviderMetadata))
		}
	}
	return origins
}

// Reasoning persisted before commits recorded its origin is attributed to the
// model its row records, so a model switch withholds it from the new model.
// The row keeps the identity of its stored version.
func TestPrepareHistoryContextAttributesReasoningToTheRowsModel(t *testing.T) {
	t.Parallel()

	relayReasoning := sdk.Message{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{
		sdk.ReasoningPart{
			ID: "0f8e2c1a", Text: "relay thought", Format: sdk.ReasoningFormatOpenAIResponses,
			ProviderMetadata: sdk.ProviderMetadata{"openai": {"itemId": "0f8e2c1a", "reasoningEncryptedContent": "RELAY"}},
		},
		sdk.TextPart{Text: "relay answer"},
	}}
	legacy := admissionHistoryMessage(t, "legacy", relayReasoning, nil)
	legacy.ModelID = "model-relay"
	unknown := admissionHistoryMessage(t, "unknown", relayReasoning, nil)
	svc := &Service{
		messageService: &memoryQueryMessageService{messages: []messagepkg.Message{
			persistedHistoryMessage(t, "question", "user", "question"), legacy, unknown,
		}},
		logger: slog.New(slog.DiscardHandler),
	}

	prepared, err := svc.prepareHistoryContext(context.Background(), ChatRequest{BotID: "bot-1", ThreadID: "session-1"}, historyfrag.ScopeFallback{}, 100000)
	if err != nil {
		t.Fatalf("prepareHistoryContext failed: %v", err)
	}
	if len(prepared.messages) != 3 {
		t.Fatalf("prepared %d messages, want 3", len(prepared.messages))
	}
	if got := historyReasoningOrigins(t, prepared.messages[1]); len(got) != 1 || got[0] != "model-relay" {
		t.Fatalf("legacy row reasoning origins = %v, want [model-relay]", got)
	}
	if got := historyReasoningOrigins(t, prepared.messages[2]); len(got) != 1 || got[0] != "" {
		t.Fatalf("a row without a model must stay unattributed, got %v", got)
	}
	stored := historyfrag.DecodeStoredModelMessage(nil, legacy.ID, legacy.Role, legacy.Content)
	if source := prepared.messages[1].ContextSource; source == nil || source.ContentHash != historyfrag.MessageContentHash(stored) {
		t.Fatalf("context source = %#v, want the stored version's hash", source)
	}
}
