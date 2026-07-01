package contextfrag

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"
)

func TestCompilePreservesTurnDAGScopeAndRenderedLegacyView(t *testing.T) {
	t.Parallel()

	scope := Scope{
		BotID:            "bot-1",
		SessionID:        "session-1",
		TurnID:           "turn-current",
		ViewHeadTurnID:   "turn-visible-head",
		TurnMessageSeq:   7,
		SessionMode:      "chat",
		RuntimeType:      "model",
		CurrentMessageID: "message-current",
		EventID:          "event-1",
	}
	messages := []sdk.Message{
		sdk.UserMessage("history"),
		sdk.AssistantMessage("reply"),
	}

	got := Compile(CompileInput{
		Source:   "run_config",
		Scope:    scope,
		System:   "system prompt",
		Messages: messages,
		Query:    "current query",
	})

	if got.System != "system prompt" {
		t.Fatalf("system = %q, want legacy system text", got.System)
	}
	if got.Query != "current query" {
		t.Fatalf("query = %q, want legacy current query", got.Query)
	}
	if len(got.Messages) != len(messages) {
		t.Fatalf("messages = %d, want %d", len(got.Messages), len(messages))
	}
	if len(got.Manifest.Items) == 0 {
		t.Fatal("manifest should describe compiled fragments")
	}
	for _, item := range got.Manifest.Items {
		if item.Scope.TurnID != "turn-current" {
			t.Fatalf("manifest item lost turn id: %#v", item.Scope)
		}
		if item.Scope.ViewHeadTurnID != "turn-visible-head" {
			t.Fatalf("manifest item lost view head turn id: %#v", item.Scope)
		}
		if item.Scope.TurnMessageSeq != 7 {
			t.Fatalf("manifest item lost turn message seq: %#v", item.Scope)
		}
		if item.Scope.SessionMode != "chat" || item.Scope.RuntimeType != "model" {
			t.Fatalf("manifest item lost session/runtime type: %#v", item.Scope)
		}
	}
}

func TestIntentProjectsToManifestView(t *testing.T) {
	t.Parallel()

	if got := IntentDiscussReply.ManifestView(); got != ViewDiscussReply {
		t.Fatalf("IntentDiscussReply.ManifestView() = %q, want %q", got, ViewDiscussReply)
	}
}

func TestNormalizeContextRefsFillsFragmentRefs(t *testing.T) {
	t.Parallel()

	frag := TextFrag(TextFragInput{
		ID:        "system.prompt",
		Kind:      KindSystemPrompt,
		Role:      sdk.MessageRoleSystem,
		Slot:      SlotSystem,
		Text:      "system prompt",
		Source:    "test",
		Collector: "test_collector",
	})

	got := NormalizeContextRefs([]ContextFrag{frag})
	if len(got) != 1 {
		t.Fatalf("NormalizeContextRefs returned %d frags, want 1", len(got))
	}
	if got[0].Ref.ID == "" {
		t.Fatal("normalized ref ID should not be empty")
	}
	if got[0].Ref.Schema != SchemaContextRef {
		t.Fatalf("normalized ref schema = %q, want %q", got[0].Ref.Schema, SchemaContextRef)
	}
	if got[0].Ref.ContentHash == "" {
		t.Fatal("normalized ref content hash should not be empty")
	}
}
