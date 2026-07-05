package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func TestHookContextCollectorProducesPinnedFrag(t *testing.T) {
	t.Parallel()

	frags, err := (&HookContextCollector{}).Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: HookContextConfig{Text: "[Hook Context: before_prompt_build]\nextra guidance"},
	})
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	frag := frags[0]
	if frag.Kind != contextfrag.KindHookContext {
		t.Fatalf("kind = %s, want hook_context", frag.Kind)
	}
	if frag.Slot != contextfrag.SlotAfterHistoryBeforeCurrent {
		t.Fatalf("slot = %s, want after_history_before_current", frag.Slot)
	}
	if frag.Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatal("hook context must be pinned against budget trimming")
	}
	msg := discussFragMessage(frag)
	if msg == nil || msg.Role != sdk.MessageRoleSystem {
		t.Fatalf("hook context must render as a system message (operator-injected context, not a user utterance): %#v", frag)
	}
}

func TestHookContextCollectorEmptyTextNoFrag(t *testing.T) {
	t.Parallel()

	frags, err := (&HookContextCollector{}).Collect(context.Background(), CollectRequest{
		Config: HookContextConfig{Text: "   "},
	})
	if err != nil || len(frags) != 0 {
		t.Fatalf("empty hook text must produce no frag: %v %d", err, len(frags))
	}
}

// TestApplyProviderRunConfigRendersHookContextBeforeQuery exercises
// ApplyProviderRunConfig's legacy (config-driven) branch: with no
// ContextSourceFrags present, the hook collector must be registered and
// invoked from cfg.ContextHookText the same way MemoryContextCollector is
// invoked from cfg.ContextMemoryText.
func TestApplyProviderRunConfigRendersHookContextBeforeQuery(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:          "system",
		Messages:        []sdk.Message{sdk.UserMessage("old history")},
		Query:           "current question",
		ContextHookText: "[Hook Context: before_prompt_build]\nextra guidance",
		ContextScope:    contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
	}
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want history + hook context + query: %#v", len(got.Messages), got.Messages)
	}
	hookText, ok := got.Messages[1].Content[0].(sdk.TextPart)
	if !ok || hookText.Text != cfg.ContextHookText {
		t.Fatalf("second message = %#v, want hook context before the query", got.Messages[1].Content)
	}
	queryText, ok := got.Messages[2].Content[0].(sdk.TextPart)
	if !ok || queryText.Text != "current question" {
		t.Fatalf("last message = %#v, want the materialized query", got.Messages[2].Content)
	}
	if strings.Contains(got.System, "extra guidance") || strings.Contains(got.System, "[Hook Context:") {
		t.Fatalf("hook context text must never leak into the system prompt: %q", got.System)
	}
	hasHookContextItem := false
	for _, item := range got.ContextManifest.Items {
		if item.Kind == contextfrag.KindHookContext {
			hasHookContextItem = true
			break
		}
	}
	if !hasHookContextItem {
		t.Fatalf("manifest items = %#v, want an item with Kind hook_context", got.ContextManifest.Items)
	}
}
