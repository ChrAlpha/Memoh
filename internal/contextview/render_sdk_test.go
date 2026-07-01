package contextview

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestSDKRenderer_SystemText(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("sys.1", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "first"),
		textFrag("sys.2", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "second"),
	}

	payload := renderSDK(t, frags, placementFor(frags))

	if payload.System != "first\n\nsecond" {
		t.Fatalf("System = %q, want joined system text", payload.System)
	}
}

func TestSDKRenderer_HistoryMessages(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("message.000", sdk.UserMessage("hello")),
		messageFrag("message.001", sdk.AssistantMessage("hi")),
	}

	payload := renderSDK(t, frags, placementFor(frags))

	assertMessagesEqual(t, payload.Messages, []sdk.Message{sdk.UserMessage("hello"), sdk.AssistantMessage("hi")})
}

func TestSDKRenderer_CurrentUserQuery(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("current.1", contextfrag.SlotCurrentUser, contextfrag.KindCurrentUserMessage, sdk.MessageRoleUser, "old"),
		textFrag("current.2", contextfrag.SlotCurrentUser, contextfrag.KindCurrentUserMessage, sdk.MessageRoleUser, "new"),
	}

	payload := renderSDK(t, frags, placementFor(frags))

	if payload.Query != "new" {
		t.Fatalf("Query = %q, want last current user text", payload.Query)
	}
}

func TestSDKRenderer_InlineImages(t *testing.T) {
	t.Parallel()

	images := []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}}
	frags := []contextfrag.ContextFrag{contextfrag.ImageFrag("current_user.images", images, contextfrag.Scope{BotID: "bot-1"}, contextfrag.SourceRunConfig)}

	payload := renderSDK(t, frags, placementFor(frags))

	assertImagesEqual(t, payload.InlineImages, images)
}

func TestSDKRenderer_ContentHashDeterministic(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("sys", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		messageFrag("message.000", sdk.UserMessage("hello")),
	}
	placement := placementFor(frags)

	first, firstHash := renderSDKPayload(t, frags, placement)
	second, secondHash := renderSDKPayload(t, frags, placement)

	if firstHash == "" {
		t.Fatal("ContentHash should be set")
	}
	if firstHash != secondHash {
		t.Fatalf("ContentHash differs: %q != %q", firstHash, secondHash)
	}
	assertMessagesEqual(t, first.Messages, second.Messages)
}

func TestSDKRenderer_EmptyInput(t *testing.T) {
	t.Parallel()

	payload, hash := renderSDKPayload(t, nil, PlacementPlan{})

	if payload.System != "" || payload.Query != "" || len(payload.Messages) != 0 || len(payload.InlineImages) != 0 {
		t.Fatalf("payload = %#v, want empty", payload)
	}
	if hash == "" {
		t.Fatal("ContentHash should be set for empty input")
	}
}

func TestSDKRenderer_SelectedWithoutPlacementReturnsError(t *testing.T) {
	t.Parallel()

	renderer := &SDKMessagesRenderer{}
	_, err := renderer.Render(context.Background(), RenderInput{
		Intent:   contextfrag.IntentRunConfigPreProvider,
		Selected: []contextfrag.ContextFrag{textFrag("sys", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system")},
		Scope:    contextfrag.Scope{BotID: "bot-1"},
		Target:   contextfrag.RenderSDKMessages,
	})
	if err == nil {
		t.Fatal("expected error for selected fragments without placement")
	}
}

func renderSDK(t *testing.T, frags []contextfrag.ContextFrag, placement PlacementPlan) *SDKRenderedPayload {
	t.Helper()
	payload, _ := renderSDKPayload(t, frags, placement)
	return payload
}

func renderSDKPayload(t *testing.T, frags []contextfrag.ContextFrag, placement PlacementPlan) (*SDKRenderedPayload, string) {
	t.Helper()
	renderer := &SDKMessagesRenderer{}
	rendered, err := renderer.Render(context.Background(), RenderInput{
		Intent:    contextfrag.IntentRunConfigPreProvider,
		Selected:  frags,
		Placement: placement,
		Scope:     contextfrag.Scope{BotID: "bot-1"},
		Target:    contextfrag.RenderSDKMessages,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	payload, ok := rendered.Data.(*SDKRenderedPayload)
	if !ok {
		t.Fatalf("Data type = %T, want *SDKRenderedPayload", rendered.Data)
	}
	return payload, rendered.ContentHash
}

func placementFor(frags []contextfrag.ContextFrag) PlacementPlan {
	items := make([]PlacementItem, len(frags))
	for i, frag := range frags {
		items[i] = PlacementItem{
			FragID:    frag.ID,
			Slot:      frag.Slot,
			Position:  i,
			CacheHint: frag.CacheClass,
			Ref:       frag.Ref,
		}
	}
	return PlacementPlan{Items: items, FirstVolatileIndex: len(items)}
}

func textFrag(id string, slot contextfrag.Slot, kind contextfrag.Kind, role sdk.MessageRole, text string) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:         id,
		Kind:       kind,
		Role:       role,
		Slot:       slot,
		Text:       text,
		Priority:   20,
		CacheClass: contextfrag.CacheStable,
		Trust:      contextfrag.TrustSystem,
		Scope:      contextfrag.Scope{BotID: "bot-1"},
		Source:     "test",
		Collector:  "test",
		Render:     contextfrag.RenderPolicy{Format: contextfrag.RenderMarkdown},
	})
}

func messageFrag(id string, msg sdk.Message) contextfrag.ContextFrag {
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         id,
		Message:    msg,
		Kind:       contextfrag.KindConversationEvent,
		Slot:       contextfrag.SlotHistory,
		Priority:   70,
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustExternal,
		Scope:      contextfrag.Scope{BotID: "bot-1"},
		Source:     "test",
		Collector:  "test",
	})
}
