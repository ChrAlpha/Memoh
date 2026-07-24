package contextview

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestAuditRenderer_CapturesManifestAndVisibleTrace(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		messageFrag("history", sdk.AssistantMessage("answer")),
	}
	placement := PlacementPlan{
		StablePrefixHash:   "stable-hash",
		FirstVolatileIndex: 1,
		Items:              placementFor(frags).Items,
	}
	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"}

	payload, rendered := renderAudit(t, RenderInput{
		Intent:    contextfrag.IntentDiscussReply,
		Scope:     scope,
		Selected:  frags,
		Placement: placement,
		Target:    contextfrag.RenderAuditManifest,
	})

	if payload.Intent != contextfrag.IntentDiscussReply {
		t.Fatalf("Intent = %q, want discuss_reply", payload.Intent)
	}
	if !reflect.DeepEqual(payload.Scope, scope) {
		t.Fatalf("Scope = %#v, want %#v", payload.Scope, scope)
	}
	if payload.SelectedCount != len(frags) {
		t.Fatalf("SelectedCount = %d, want %d", payload.SelectedCount, len(frags))
	}
	if len(payload.Manifest.Items) != len(frags) {
		t.Fatalf("Manifest items = %d, want %d", len(payload.Manifest.Items), len(frags))
	}
	if payload.Manifest.Items[0].ID != "system" || payload.Manifest.Items[1].ID != "history" {
		t.Fatalf("Manifest item ids = %#v, want selected fragment ids", payload.Manifest.Items)
	}
	if payload.Manifest.View != contextfrag.ViewDiscussReply {
		t.Fatalf("Manifest view = %q, want discuss_reply", payload.Manifest.View)
	}
	if !reflect.DeepEqual(payload.PlacementPlan, placement) {
		t.Fatalf("PlacementPlan = %#v, want %#v", payload.PlacementPlan, placement)
	}
	if payload.ContentHash == "" || rendered.ContentHash != payload.ContentHash {
		t.Fatalf("ContentHash = %q outer=%q, want matching non-empty hashes", payload.ContentHash, rendered.ContentHash)
	}
}

func TestAuditRenderer_ContentHashDeterministic(t *testing.T) {
	t.Parallel()

	input := RenderInput{
		Intent:    contextfrag.IntentACPRuntimePrompt,
		Scope:     contextfrag.Scope{BotID: "bot-1"},
		Selected:  []contextfrag.ContextFrag{textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system")},
		Placement: placementFor([]contextfrag.ContextFrag{textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system")}),
		Target:    contextfrag.RenderAuditManifest,
	}
	first, _ := renderAudit(t, input)
	second, _ := renderAudit(t, input)

	if first.ContentHash != second.ContentHash {
		t.Fatalf("ContentHash not deterministic: first=%q second=%q", first.ContentHash, second.ContentHash)
	}
}

func TestAuditRenderer_ContentHashNormalizesEmptyPlacement(t *testing.T) {
	t.Parallel()

	base := RenderInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Target: contextfrag.RenderAuditManifest,
	}
	nilPlacement, _ := renderAudit(t, base)
	emptyPlacement, _ := renderAudit(t, RenderInput{
		Intent:    base.Intent,
		Scope:     base.Scope,
		Placement: PlacementPlan{Items: []PlacementItem{}},
		Target:    base.Target,
	})

	if nilPlacement.ContentHash != emptyPlacement.ContentHash {
		t.Fatalf("ContentHash differs for nil vs empty placement: nil=%q empty=%q", nilPlacement.ContentHash, emptyPlacement.ContentHash)
	}
}

func TestAuditRenderer_EmptyInput(t *testing.T) {
	t.Parallel()

	payload, rendered := renderAudit(t, RenderInput{Target: contextfrag.RenderAuditManifest})

	if payload.Intent != "" {
		t.Fatalf("Intent = %q, want zero", payload.Intent)
	}
	if payload.SelectedCount != 0 {
		t.Fatalf("SelectedCount = %d, want zero", payload.SelectedCount)
	}
	if len(payload.Manifest.Items) != 0 {
		t.Fatalf("Manifest items = %d, want zero", len(payload.Manifest.Items))
	}
	if len(payload.PlacementPlan.Items) != 0 {
		t.Fatalf("Placement items = %d, want zero", len(payload.PlacementPlan.Items))
	}
	if payload.ContentHash == "" || rendered.ContentHash != payload.ContentHash {
		t.Fatalf("ContentHash = %q outer=%q, want matching non-empty hashes", payload.ContentHash, rendered.ContentHash)
	}
}

func TestAuditRenderer_ScopeAndIntent(t *testing.T) {
	t.Parallel()

	scope := contextfrag.Scope{BotID: "bot-1", ChatID: "chat-1", SessionID: "session-1"}
	payload, _ := renderAudit(t, RenderInput{
		Intent: contextfrag.IntentACPRuntimePrompt,
		Scope:  scope,
		Target: contextfrag.RenderAuditManifest,
	})

	if payload.Intent != contextfrag.IntentACPRuntimePrompt {
		t.Fatalf("Intent = %q, want ACP runtime prompt", payload.Intent)
	}
	if !reflect.DeepEqual(payload.Scope, scope) {
		t.Fatalf("Scope = %#v, want %#v", payload.Scope, scope)
	}
	if payload.Manifest.View != contextfrag.ViewACPRuntimePrompt {
		t.Fatalf("Manifest view = %q, want ACP runtime prompt", payload.Manifest.View)
	}
}

func TestAuditRenderer_PlacementDetails(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		textFrag("system", contextfrag.SlotSystem, contextfrag.KindSystemPrompt, sdk.MessageRoleSystem, "system"),
		messageFrag("history", sdk.AssistantMessage("answer")),
		messageFrag("current", sdk.UserMessage("question")),
	}
	placement := placementFor(frags)
	payload, _ := renderAudit(t, RenderInput{
		Intent:    contextfrag.IntentDiscussReply,
		Selected:  frags,
		Placement: placement,
		Target:    contextfrag.RenderAuditManifest,
	})

	if len(payload.PlacementPlan.Items) != len(frags) {
		t.Fatalf("Placement items = %d, want %d", len(payload.PlacementPlan.Items), len(frags))
	}
	for i, item := range payload.PlacementPlan.Items {
		if item.FragID != frags[i].ID || item.Position != i {
			t.Fatalf("placement item %d = %#v, want frag %q at position %d", i, item, frags[i].ID, i)
		}
	}
}

func renderAudit(t *testing.T, input RenderInput) (*AuditManifestPayload, RenderedPayload) {
	t.Helper()
	rendered, err := (&AuditManifestRenderer{}).Render(context.Background(), input)
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	payload, ok := rendered.Data.(*AuditManifestPayload)
	if !ok {
		t.Fatalf("Data type = %T, want *AuditManifestPayload", rendered.Data)
	}
	if rendered.Target != contextfrag.RenderAuditManifest {
		t.Fatalf("Target = %q, want %q", rendered.Target, contextfrag.RenderAuditManifest)
	}
	return payload, rendered
}
