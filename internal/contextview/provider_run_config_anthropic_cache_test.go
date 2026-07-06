package contextview

import (
	"context"
	"testing"

	anthropicmessages "github.com/memohai/twilight-ai/provider/anthropic/messages"
	openaicompletions "github.com/memohai/twilight-ai/provider/openai/completions"
	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/models"
)

func anthropicCacheTestModel() *sdk.Model {
	provider := anthropicmessages.New(anthropicmessages.WithAPIKey("test"))
	return provider.ChatModel("claude-test")
}

func openAICacheTestModel() *sdk.Model {
	provider := openaicompletions.New(openaicompletions.WithAPIKey("test"))
	return provider.ChatModel("gpt-test")
}

func lastTextCacheControl(msg sdk.Message) *sdk.CacheControl {
	if len(msg.Content) == 0 {
		return nil
	}
	text, ok := msg.Content[len(msg.Content)-1].(sdk.TextPart)
	if !ok {
		return nil
	}
	return text.CacheControl
}

// TestApplyProviderRunConfigAnthropicMessageLevelBreakpoint proves the fix end
// to end: a StableMessageCount produced by the context view (history frags are
// now CacheStable) actually reaches models.ApplyPromptCacheWithPlan and lands a
// cache_control breakpoint on the last stable history message, in addition to
// the pre-existing system and last-tool breakpoints Anthropic already got.
func TestApplyProviderRunConfigAnthropicMessageLevelBreakpoint(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System: "stable system prompt",
		Messages: []sdk.Message{
			sdk.UserMessage("h1"),
			sdk.AssistantMessage("h2"),
		},
		Query: "current question",
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if got.ContextCachePlan.StableMessageCount != 2 {
		t.Fatalf("stable message count = %d, want 2", got.ContextCachePlan.StableMessageCount)
	}

	tools := []sdk.Tool{{Name: "search"}}
	model := anthropicCacheTestModel()
	newSystem, newMessages, newTools, systemPrepended := models.ApplyPromptCacheWithPlan(model, models.DefaultPromptCacheTTL, got.ContextCachePlan, got.System, got.Messages, tools)

	breakpoints := 0

	if !systemPrepended {
		t.Fatal("expected the system prompt to be promoted into a message for Anthropic")
	}
	if newSystem != "" {
		t.Fatalf("system should be cleared after promotion, got %q", newSystem)
	}
	systemCC := lastTextCacheControl(newMessages[0])
	if systemCC == nil {
		t.Fatal("promoted system message should carry a cache breakpoint")
	}
	breakpoints++

	shift := 0
	if systemPrepended {
		shift = 1
	}
	stableIndex := got.ContextCachePlan.StableMessageCount - 1 + shift
	stableCC := lastTextCacheControl(newMessages[stableIndex])
	if stableCC == nil {
		t.Fatalf("last stable history message (index %d) should carry a cache breakpoint", stableIndex)
	}
	breakpoints++

	if len(newTools) == 0 || newTools[len(newTools)-1].CacheControl == nil {
		t.Fatal("last tool should carry a cache breakpoint")
	}
	breakpoints++

	const anthropicBreakpointLimit = 4
	if breakpoints != 3 {
		t.Fatalf("breakpoints applied = %d, want 3 (system + last-tool + last-history-message)", breakpoints)
	}
	if breakpoints > anthropicBreakpointLimit {
		t.Fatalf("breakpoints applied = %d, exceeds Anthropic's hard limit of %d", breakpoints, anthropicBreakpointLimit)
	}
}

// TestApplyProviderRunConfigNonAnthropicUnaffected proves a non-Anthropic
// model sees zero change from the fix: a non-zero StableMessageCount is
// produced by the context view, but ApplyPromptCacheWithPlan only implements
// cache decoration for Anthropic Messages, so the rendered messages must stay
// byte-identical to the pre-cache input.
func TestApplyProviderRunConfigNonAnthropicUnaffected(t *testing.T) {
	t.Parallel()

	cfg := agentpkg.RunConfig{
		System: "stable system prompt",
		Messages: []sdk.Message{
			sdk.UserMessage("h1"),
			sdk.AssistantMessage("h2"),
		},
		Query: "current question",
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "session-1",
		},
	}

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if got.ContextCachePlan.StableMessageCount == 0 {
		t.Fatal("test fixture must produce a non-zero stable message count")
	}

	tools := []sdk.Tool{{Name: "search"}}
	model := openAICacheTestModel()
	newSystem, newMessages, newTools, systemPrepended := models.ApplyPromptCacheWithPlan(model, models.DefaultPromptCacheTTL, got.ContextCachePlan, got.System, got.Messages, tools)

	if systemPrepended {
		t.Fatal("non-Anthropic models must not get system promotion")
	}
	if newSystem != got.System {
		t.Fatalf("system = %q, want unchanged %q", newSystem, got.System)
	}
	if !sdkMessagesJSONEqual(newMessages, got.Messages) {
		t.Fatal("messages should be byte-identical to input for a non-Anthropic model")
	}
	for i, tool := range newTools {
		if tool.CacheControl != nil {
			t.Fatalf("tool %d should not carry a cache breakpoint for a non-Anthropic model", i)
		}
	}
}
