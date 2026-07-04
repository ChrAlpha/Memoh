package contextview

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func TestMemoryContextCollectorProducesPinnedRecallFrag(t *testing.T) {
	t.Parallel()

	frags, err := (&MemoryContextCollector{}).Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: MemoryContextConfig{Text: "user prefers dark mode"},
	})
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(frags) != 1 {
		t.Fatalf("frags = %d, want 1", len(frags))
	}
	frag := frags[0]
	if frag.Kind != contextfrag.KindMemoryRecall {
		t.Fatalf("kind = %s, want memory_recall", frag.Kind)
	}
	if frag.Budget.Overflow != contextfrag.OverflowKeep {
		t.Fatal("memory recall must be pinned against budget trimming")
	}
	msg := discussFragMessage(frag)
	if msg == nil || msg.Role != sdk.MessageRoleUser {
		t.Fatalf("memory recall renders as a user message: %#v", frag)
	}
}

func TestMemoryContextCollectorEmptyTextNoFrag(t *testing.T) {
	t.Parallel()

	frags, err := (&MemoryContextCollector{}).Collect(context.Background(), CollectRequest{
		Config: MemoryContextConfig{Text: "   "},
	})
	if err != nil || len(frags) != 0 {
		t.Fatalf("empty memory text must produce no frag: %v %d", err, len(frags))
	}
}

func TestApplyProviderRunConfigRendersMemoryBeforeQuery(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System:            "system",
		Messages:          []sdk.Message{sdk.UserMessage("old history")},
		Query:             "current question",
		ContextMemoryText: "remembered fact",
		ContextScope:      contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
	}
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want history + memory + query", len(got.Messages))
	}
	memoryText, ok := got.Messages[1].Content[0].(sdk.TextPart)
	if !ok || memoryText.Text != "remembered fact" {
		t.Fatalf("second message = %#v, want memory recall before the query", got.Messages[1].Content)
	}
	queryText, ok := got.Messages[2].Content[0].(sdk.TextPart)
	if !ok || queryText.Text != "current question" {
		t.Fatalf("last message = %#v, want the materialized query", got.Messages[2].Content)
	}
	if !got.ContextQueryMaterialized {
		t.Fatal("the view must own query materialization")
	}
}
