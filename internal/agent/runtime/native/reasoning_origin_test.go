package native

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/partmeta"
	"github.com/felinics/memoh/internal/agent/step"
)

func reasoningIssuedBy(modelUUID, text string) sdk.ReasoningPart {
	return stampReasoningOrigin([]sdk.ReasoningPart{{
		ID:               "rs_" + text,
		Text:             text,
		Format:           sdk.ReasoningFormatOpenAIResponses,
		ProviderMetadata: sdk.ProviderMetadata{"openai": {"itemId": "rs_" + text, "reasoningEncryptedContent": "EC-" + text}},
	}}, modelUUID)[0]
}

func replayedReasoningTexts(messages []sdk.Message) []string {
	var texts []string
	for _, msg := range messages {
		for _, part := range msg.Content {
			if p, ok := part.(sdk.ReasoningPart); ok {
				texts = append(texts, p.Text)
			}
		}
	}
	return texts
}

func TestStampReasoningOriginRecordsTheIssuingModel(t *testing.T) {
	t.Parallel()

	parts := []sdk.ReasoningPart{{Text: "thought", ProviderMetadata: sdk.ProviderMetadata{"anthropic": {"signature": "SIG"}}}}
	stamped := stampReasoningOrigin(parts, "model-a")
	if got := partmeta.ReasoningOrigin(stamped[0].ProviderMetadata); got != "model-a" {
		t.Fatalf("origin = %q, want model-a", got)
	}
	if stamped[0].ProviderMetadata.Get("anthropic", "signature") != "SIG" {
		t.Fatalf("provider token lost: %#v", stamped[0].ProviderMetadata)
	}
	if partmeta.ReasoningOrigin(parts[0].ProviderMetadata) != "" {
		t.Fatal("stamping modified the input parts")
	}
	if got := stampReasoningOrigin(parts, ""); !reflect.DeepEqual(got, parts) {
		t.Fatalf("an unknown model must record nothing: %#v", got)
	}
}

func TestWithholdForeignReasoning(t *testing.T) {
	t.Parallel()

	unrecorded := sdk.ReasoningPart{Text: "unrecorded", Format: sdk.ReasoningFormatAnthropic}
	messages := []sdk.Message{
		sdk.UserMessage("first"),
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{reasoningIssuedBy("model-a", "a"), sdk.TextPart{Text: "answer a"}}},
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{reasoningIssuedBy("model-b", "b"), sdk.TextPart{Text: "answer b"}}},
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{unrecorded, sdk.TextPart{Text: "answer legacy"}}},
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{reasoningIssuedBy("model-a", "only")}},
		sdk.UserMessage("next"),
	}
	original := append([]sdk.Message(nil), messages...)

	got := withholdForeignReasoning(messages, "model-b")

	if texts := replayedReasoningTexts(got); !reflect.DeepEqual(texts, []string{"b", "unrecorded"}) {
		t.Fatalf("replayed reasoning = %v, want the current model's and the unrecorded part", texts)
	}
	if len(got) != len(messages)-1 {
		t.Fatalf("messages = %d, want %d: an assistant turn left empty is dropped", len(got), len(messages)-1)
	}
	if text := messageContentText(got[1]); text != "answer a" {
		t.Fatalf("assistant text beside withheld reasoning = %q, want it kept", text)
	}
	if !reflect.DeepEqual(messages, original) {
		t.Fatal("withholding modified the input messages")
	}
	if same := withholdForeignReasoning(messages, ""); !reflect.DeepEqual(same, messages) {
		t.Fatal("an unknown model must withhold nothing")
	}
	if own := messages[2:3]; !reflect.DeepEqual(withholdForeignReasoning(own, "model-b"), own) {
		t.Fatal("messages without foreign reasoning must pass through unchanged")
	}
}

// A run commits its reasoning under the model it ran on and sends none of the
// reasoning another model issued; replaying the committed step keeps it for
// the same model and withholds it from a different one.
func TestAgentStreamRecordsAndWithholdsReasoningByModel(t *testing.T) {
	t.Parallel()

	var sent []sdk.Request
	provider := agentStreamTestProvider(func(_ context.Context, params sdk.Request) (<-chan sdk.StreamPart, error) {
		sent = append(sent, params)
		return closedAgentTestStream(
			&sdk.StartPart{},
			&sdk.StartStepPart{},
			&sdk.ReasoningStartPart{ID: "rs_new", Format: sdk.ReasoningFormatOpenAIResponses},
			&sdk.ReasoningDeltaPart{ID: "rs_new", Text: "new", Format: sdk.ReasoningFormatOpenAIResponses},
			&sdk.ReasoningEndPart{
				ID: "rs_new", Format: sdk.ReasoningFormatOpenAIResponses,
				ProviderMetadata: sdk.ProviderMetadata{"openai": {"itemId": "rs_new", "reasoningEncryptedContent": "EC-new"}},
			},
			&sdk.TextDeltaPart{ID: "text", Text: "done"},
			&sdk.FinishStepPart{FinishReason: sdk.FinishReasonStop},
			&sdk.FinishPart{FinishReason: sdk.FinishReasonStop},
		), nil
	})
	run := func(modelUUID string, messages []sdk.Message) []sdk.Message {
		var committed []sdk.Message
		events := New(Deps{}).Stream(context.Background(), RunConfig{
			Model:            &sdk.Model{ID: "mock-model", Provider: provider},
			CurrentModelUUID: modelUUID,
			Messages:         messages,
			Identity:         SessionContext{BotID: "bot-1"},
			OnStepCommitted: func(_ context.Context, _ int, record *step.Record) (StepDirective, error) {
				committed = append(committed, record.Messages...)
				return StepDirective{}, nil
			},
		})
		for range events {
		}
		return committed
	}

	history := []sdk.Message{
		sdk.UserMessage("first"),
		{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{reasoningIssuedBy("model-a", "a"), sdk.TextPart{Text: "answer a"}}},
		sdk.UserMessage("second"),
	}
	committed := run("model-b", history)

	if texts := replayedReasoningTexts(sent[0].Messages); len(texts) != 0 {
		t.Fatalf("model-b was sent model-a's reasoning: %v", texts)
	}
	var reasoning []sdk.ReasoningPart
	for _, msg := range committed {
		for _, part := range msg.Content {
			if p, ok := part.(sdk.ReasoningPart); ok {
				reasoning = append(reasoning, p)
			}
		}
	}
	if len(reasoning) != 1 || partmeta.ReasoningOrigin(reasoning[0].ProviderMetadata) != "model-b" {
		t.Fatalf("committed reasoning = %#v, want one part issued by model-b", reasoning)
	}
	if reasoning[0].ProviderMetadata.Get("openai", "reasoningEncryptedContent") != "EC-new" {
		t.Fatalf("provider token lost on commit: %#v", reasoning[0].ProviderMetadata)
	}

	continued := append(append(append([]sdk.Message(nil), history...), committed...), sdk.UserMessage("third"))
	run("model-b", continued)
	if texts := replayedReasoningTexts(sent[1].Messages); !reflect.DeepEqual(texts, []string{"new"}) {
		t.Fatalf("model-b replayed %v, want its own reasoning", texts)
	}
	run("model-c", continued)
	if texts := replayedReasoningTexts(sent[2].Messages); len(texts) != 0 {
		t.Fatalf("model-c was sent other models' reasoning: %v", texts)
	}
}

func TestAgentGenerateWithholdsForeignReasoning(t *testing.T) {
	t.Parallel()

	provider := &recordingReasoningProvider{}
	_, err := New(Deps{}).Generate(context.Background(), RunConfig{
		Model:            &sdk.Model{ID: "mock-model", Provider: provider},
		CurrentModelUUID: "model-b",
		Messages: []sdk.Message{
			sdk.UserMessage("first"),
			{Role: sdk.MessageRoleAssistant, Content: []sdk.MessagePart{reasoningIssuedBy("model-a", "a"), sdk.TextPart{Text: "answer a"}}},
			sdk.UserMessage("second"),
		},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if texts := replayedReasoningTexts(provider.params.Messages); len(texts) != 0 {
		t.Fatalf("model-b was sent model-a's reasoning: %v", texts)
	}
}

func TestReasoningOriginSurvivesTheStoredShape(t *testing.T) {
	t.Parallel()

	part := reasoningIssuedBy("model-a", "a")
	stored := partmeta.Unfold(part.ProviderMetadata)
	if origin, ok := stored[partmeta.KeyReasoningOrigin].(map[string]any); !ok || origin["model_uuid"] != "model-a" {
		t.Fatalf("stored origin = %#v, want a top-level object", stored[partmeta.KeyReasoningOrigin])
	}
	part.ProviderMetadata = partmeta.Fold(stored)
	if got := partmeta.ReasoningOrigin(part.ProviderMetadata); got != "model-a" {
		t.Fatalf("origin after a round trip = %q, want model-a", got)
	}
	if part.ProviderMetadata.Get("openai", "itemId") != "rs_a" {
		t.Fatalf("provider namespace after a round trip: %#v", part.ProviderMetadata)
	}
}
