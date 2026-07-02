package models

import (
	"testing"

	anthropicmessages "github.com/memohai/twilight-ai/provider/anthropic/messages"
	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

func anthropicTestModel() *sdk.Model {
	provider := anthropicmessages.New(anthropicmessages.WithAPIKey("test"))
	return provider.ChatModel("claude-test")
}

func TestApplyPromptCacheZeroPlanMatchesLegacy(t *testing.T) {
	t.Parallel()

	model := anthropicTestModel()
	messages := []sdk.Message{sdk.UserMessage("hello"), sdk.AssistantMessage("hi")}
	tools := []sdk.Tool{{Name: "calc"}}

	legacySystem, legacyMessages, legacyTools := ApplyPromptCache(model, "5m", "system", messages, tools)
	planSystem, planMessages, planTools := ApplyPromptCacheWithPlan(model, "5m", contextfrag.CachePlan{}, "system", messages, tools)

	if legacySystem != planSystem || len(legacyMessages) != len(planMessages) || len(legacyTools) != len(planTools) {
		t.Fatal("zero plan must preserve the legacy cache layout")
	}
	for i := range legacyMessages {
		if len(legacyMessages[i].Content) != len(planMessages[i].Content) {
			t.Fatalf("message %d content diverged", i)
		}
	}
}

func TestApplyPromptCachePlanSetsStableMessageBreakpoint(t *testing.T) {
	t.Parallel()

	model := anthropicTestModel()
	messages := []sdk.Message{
		sdk.UserMessage("stable summary"),
		sdk.UserMessage("volatile question"),
	}
	plan := contextfrag.CachePlan{StablePrefixHash: "abc", StableMessageCount: 1}

	_, got, _ := ApplyPromptCacheWithPlan(model, "5m", plan, "system", messages, nil)

	// got[0] is the relocated system message; got[1] is the stable message.
	stable, ok := got[1].Content[len(got[1].Content)-1].(sdk.TextPart)
	if !ok || stable.CacheControl == nil {
		t.Fatalf("stable message should carry cache breakpoint: %#v", got[1].Content)
	}
	volatilePart, ok := got[2].Content[len(got[2].Content)-1].(sdk.TextPart)
	if !ok || volatilePart.CacheControl != nil {
		t.Fatalf("volatile message must not carry cache breakpoint: %#v", got[2].Content)
	}
}

func TestApplyPromptCachePlanOutOfRangeIgnored(t *testing.T) {
	t.Parallel()

	model := anthropicTestModel()
	messages := []sdk.Message{sdk.UserMessage("only")}
	plan := contextfrag.CachePlan{StableMessageCount: 5}

	_, got, _ := ApplyPromptCacheWithPlan(model, "5m", plan, "", messages, nil)
	part, ok := got[0].Content[0].(sdk.TextPart)
	if !ok || part.CacheControl != nil {
		t.Fatalf("out-of-range plan must be ignored: %#v", got[0].Content)
	}
}
