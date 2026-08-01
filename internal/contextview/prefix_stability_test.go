package contextview

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func prefixSystemFrags() []contextfrag.ContextFrag {
	return []contextfrag.ContextFrag{
		systemTextFrag("system.prompt", "base", contextfrag.KindSystemPrompt, 20),
		systemTextFrag("system.workspace", "workspace", contextfrag.KindWorkspaceInstruction, 50),
	}
}

func TestApplyProviderRunConfigCrossTurnPrefixStable(t *testing.T) {
	t.Parallel()
	turn1Frags := append(prefixSystemFrags(), stableHistoryMessageFrag("message.000", sdk.UserMessage("history")), currentMessageFrag("message.001", "q1"))
	turn2Frags := append(prefixSystemFrags(),
		stableHistoryMessageFrag("message.000", sdk.UserMessage("history")),
		stableHistoryMessageFrag("message.001", sdk.UserMessage("q1")),
		stableHistoryMessageFrag("message.002", sdk.AssistantMessage("a1")),
		currentMessageFrag("message.003", "q2"),
	)
	turn1 := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{ContextSourceFrags: turn1Frags})
	turn2 := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{ContextSourceFrags: turn2Frags})
	if turn1.System != turn2.System || !reflect.DeepEqual(turn1.Messages, turn2.Messages[:len(turn1.Messages)]) {
		t.Fatalf("turn1 = %#v, turn2 = %#v", turn1, turn2)
	}
}

func TestApplyProviderRunConfigMemoryAndHookIsolation(t *testing.T) {
	t.Parallel()
	build := func(memory, hook string) agentpkg.RunConfig {
		frags := prefixSystemFrags()
		if hook != "" {
			hookFrags, err := (&HookContextCollector{}).Collect(context.Background(), CollectRequest{Config: HookContextConfig{Text: hook}})
			if err != nil {
				t.Fatal(err)
			}
			frags = append(frags, hookFrags...)
		}
		frags = append(frags, stableHistoryMessageFrag("history", sdk.UserMessage("history")))
		if memory != "" {
			memoryFrags, err := (&MemoryContextCollector{}).Collect(context.Background(), CollectRequest{Config: MemoryContextConfig{Text: memory}})
			if err != nil {
				t.Fatal(err)
			}
			frags = append(frags, memoryFrags...)
		}
		frags = append(frags, currentMessageFrag("current", "question"))
		return agentpkg.RunConfig{ContextSourceFrags: frags}
	}
	first := ApplyProviderRunConfig(context.Background(), nil, build("memory one", "hook one"))
	second := ApplyProviderRunConfig(context.Background(), nil, build("memory two", "hook two"))
	if first.ContextCachePlan.StablePrefixHash != second.ContextCachePlan.StablePrefixHash {
		t.Fatalf("stable hashes = %q, %q", first.ContextCachePlan.StablePrefixHash, second.ContextCachePlan.StablePrefixHash)
	}
	if first.System == second.System {
		t.Fatal("legacy prompt hook bytes must remain in System")
	}
	if first.Messages[1].Content[0].(sdk.TextPart).Text != "memory one" || second.Messages[1].Content[0].(sdk.TextPart).Text != "memory two" {
		t.Fatalf("messages = %#v / %#v", first.Messages, second.Messages)
	}
	if first.ContextCachePlan.StableMessageCount != 0 {
		t.Fatalf("stable message count = %d, want volatile hook to end prefix", first.ContextCachePlan.StableMessageCount)
	}
}

func TestApplyProviderRunConfigSameTurnIdempotent(t *testing.T) {
	t.Parallel()
	cfg := fragsFirstFixture()
	first := ApplyProviderRunConfig(context.Background(), nil, cfg)
	second := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if first.System != second.System || !reflect.DeepEqual(first.Messages, second.Messages) || !reflect.DeepEqual(first.ContextManifest.Items, second.ContextManifest.Items) {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
}
