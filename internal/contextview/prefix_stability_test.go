package contextview

import (
	"context"
	"reflect"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

// Fixture builders shared by the prefix-stability tests below. Each call
// returns a fresh slice/value so tests that build "the same frags again"
// (next turn, or a repeated call) get byte-identical but independently
// allocated fragments, the way two real resolver passes would.

func prefixStabilityScope() contextfrag.Scope {
	return contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
}

func prefixStabilitySystemFrags() []contextfrag.ContextFrag {
	return []contextfrag.ContextFrag{
		systemTextFrag("system.prompt", "base system", contextfrag.KindSystemPrompt, 20),
		systemTextFrag("system.workspace_instructions", "workspace text", contextfrag.KindWorkspaceInstruction, 50),
	}
}

func prefixStabilityHistoryFrags() []contextfrag.ContextFrag {
	return []contextfrag.ContextFrag{
		historyMessageFrag("message.000", sdk.UserMessage("history question")),
		historyMessageFrag("message.001", sdk.AssistantMessage("history answer")),
	}
}

func currentUserTextFrag(text string) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID:         "current_user.message",
		Kind:       contextfrag.KindCurrentUserMessage,
		Role:       sdk.MessageRoleUser,
		Slot:       contextfrag.SlotCurrentUser,
		Text:       text,
		Priority:   90,
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustUser,
		Scope:      contextfrag.Scope{BotID: "bot-1"},
		Source:     contextfrag.SourceRunConfig,
		Collector:  "current_user",
	})
}

// memoryRecallFrag mirrors MemoryContextCollector.Collect (collector_memory.go)
// field-for-field, so the fixture matches what the resolver actually produces.
func memoryRecallFrag(text string) contextfrag.ContextFrag {
	msg := sdk.UserMessage(text)
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "memory.recall",
		Message:    msg,
		Kind:       contextfrag.KindMemoryRecall,
		Slot:       contextfrag.SlotHistory,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustSystem,
		Scope:      contextfrag.Scope{BotID: "bot-1"},
		Source:     "memory_context",
		SourceID:   "recall",
		Collector:  "memory_context",
		Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
}

// hookContextFrag mirrors HookContextCollector.Collect (collector_hook.go)
// field-for-field, so the fixture matches what the resolver actually produces.
func hookContextFrag(text string) contextfrag.ContextFrag {
	msg := sdk.UserMessage(text)
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "hook_context.message",
		Message:    msg,
		Kind:       contextfrag.KindHookContext,
		Slot:       contextfrag.SlotAfterHistoryBeforeCurrent,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustSystem,
		Scope:      contextfrag.Scope{BotID: "bot-1"},
		Source:     "hook_context",
		SourceID:   "hook_context",
		Collector:  "hook_context",
		Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
}

// messagesPreservePrefix mirrors stepSelectionPreservesPrefix
// (internal/agent/agent.go): before must be an exact message-wise prefix of
// after.
func messagesPreservePrefix(before, after []sdk.Message) bool {
	if len(before) > len(after) {
		return false
	}
	return reflect.DeepEqual(before, after[:len(before)])
}

// TestApplyProviderRunConfigCrossTurnPrefixStable locks the append-only
// invariant: once turn1 has rendered a System string and a Messages stream,
// turn2 (same system frags, same original history, history grown by turn1's
// own exchange, plus a new question) must reuse that exact System and must
// only ever append to that exact Messages prefix.
func TestApplyProviderRunConfigCrossTurnPrefixStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	turn1Frags := prefixStabilitySystemFrags()
	turn1Frags = append(turn1Frags, prefixStabilityHistoryFrags()...)
	turn1Frags = append(turn1Frags, currentUserTextFrag("q1"))
	turn1 := ApplyProviderRunConfig(ctx, nil, agentpkg.RunConfig{
		ContextSourceFrags: turn1Frags,
		ContextScope:       prefixStabilityScope(),
	})

	turn2Frags := prefixStabilitySystemFrags()
	turn2Frags = append(turn2Frags, prefixStabilityHistoryFrags()...)
	turn2Frags = append(turn2Frags,
		historyMessageFrag("message.002", sdk.UserMessage("q1")),
		historyMessageFrag("message.003", sdk.AssistantMessage("a1")),
	)
	turn2Frags = append(turn2Frags, currentUserTextFrag("q2"))
	turn2 := ApplyProviderRunConfig(ctx, nil, agentpkg.RunConfig{
		ContextSourceFrags: turn2Frags,
		ContextScope:       prefixStabilityScope(),
	})

	if turn2.System != turn1.System {
		t.Fatalf("system prompt must stay byte-identical across turns when the system frags are unchanged:\nturn1 = %q\nturn2 = %q", turn1.System, turn2.System)
	}
	if len(turn2.Messages) < len(turn1.Messages) {
		t.Fatalf("turn2 must contain at least as many messages as turn1 (append-only): turn1=%d turn2=%d", len(turn1.Messages), len(turn2.Messages))
	}
	if !messagesPreservePrefix(turn1.Messages, turn2.Messages) {
		t.Fatalf("turn1 messages must be an exact prefix of turn2 messages:\nturn1 = %#v\nturn2[:len(turn1)] = %#v", turn1.Messages, turn2.Messages[:len(turn1.Messages)])
	}
}

// TestApplyProviderRunConfigMemoryRecallSystemIsolation checks the specific,
// real invariant memory recall must satisfy: it can never leak into System,
// and identical memory text renders fully reproducible output. It does not
// assert that Messages stays an index-wise prefix across turns when memory is
// present: memory is a history-slot fragment collected after "real" history
// (see CollectProviderSourceFrags) and pinned immediately before the current
// request, so its array position necessarily shifts forward as history grows
// between turns. That is intentional (see commit bd6ab906), not a bug.
func TestApplyProviderRunConfigMemoryRecallSystemIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	withMemory := func(memoryText string) agentpkg.RunConfig {
		frags := prefixStabilitySystemFrags()
		frags = append(frags, prefixStabilityHistoryFrags()...)
		frags = append(frags, memoryRecallFrag(memoryText))
		frags = append(frags, currentUserTextFrag("current question"))
		return agentpkg.RunConfig{ContextSourceFrags: frags, ContextScope: prefixStabilityScope()}
	}

	got1 := ApplyProviderRunConfig(ctx, nil, withMemory("remembered fact"))
	got2 := ApplyProviderRunConfig(ctx, nil, withMemory("remembered fact"))

	if got1.System != got2.System {
		t.Fatalf("identical fixtures must render byte-identical system prompts:\ngot1 = %q\ngot2 = %q", got1.System, got2.System)
	}
	if !reflect.DeepEqual(got1.Messages, got2.Messages) {
		t.Fatalf("identical fixtures must render identical messages:\ngot1 = %#v\ngot2 = %#v", got1.Messages, got2.Messages)
	}

	got3 := ApplyProviderRunConfig(ctx, nil, withMemory("a completely different remembered fact"))
	if got3.System != got1.System {
		t.Fatalf("memory recall text must never leak into the system prompt:\ngot1.System = %q\ngot3.System = %q", got1.System, got3.System)
	}
}

// TestApplyProviderRunConfigHookContextSystemIsolation checks the specific,
// real invariant hook context must satisfy: it can never leak into System,
// and identical hook text renders fully reproducible output. Like memory
// recall, hook context is a history-adjacent message fragment (not system),
// so its content must never affect the cacheable system prefix.
func TestApplyProviderRunConfigHookContextSystemIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	withHookContext := func(hookText string) agentpkg.RunConfig {
		frags := prefixStabilitySystemFrags()
		frags = append(frags, prefixStabilityHistoryFrags()...)
		frags = append(frags, hookContextFrag(hookText))
		frags = append(frags, currentUserTextFrag("current question"))
		return agentpkg.RunConfig{ContextSourceFrags: frags, ContextScope: prefixStabilityScope()}
	}

	got1 := ApplyProviderRunConfig(ctx, nil, withHookContext("hook says hi"))
	got2 := ApplyProviderRunConfig(ctx, nil, withHookContext("hook says hi"))

	if got1.System != got2.System {
		t.Fatalf("identical fixtures must render byte-identical system prompts:\ngot1 = %q\ngot2 = %q", got1.System, got2.System)
	}
	if !reflect.DeepEqual(got1.Messages, got2.Messages) {
		t.Fatalf("identical fixtures must render identical messages:\ngot1 = %#v\ngot2 = %#v", got1.Messages, got2.Messages)
	}

	got3 := ApplyProviderRunConfig(ctx, nil, withHookContext("a completely different hook message"))
	if got3.System != got1.System {
		t.Fatalf("hook context text must never leak into the system prompt:\ngot1.System = %q\ngot3.System = %q", got1.System, got3.System)
	}
}

// TestApplyProviderRunConfigSameTurnIdempotent calls ApplyProviderRunConfig
// twice on the very same RunConfig value (not the previous call's return),
// to prove the function has no hidden mutation of shared state and no
// non-deterministic ordering (e.g. a map range) leaking into the render or
// the manifest.
func TestApplyProviderRunConfigSameTurnIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	frags := prefixStabilitySystemFrags()
	frags = append(frags, prefixStabilityHistoryFrags()...)
	frags = append(frags, currentUserTextFrag("idempotent question"))
	cfg := agentpkg.RunConfig{ContextSourceFrags: frags, ContextScope: prefixStabilityScope()}

	first := ApplyProviderRunConfig(ctx, nil, cfg)
	second := ApplyProviderRunConfig(ctx, nil, cfg)

	if first.System != second.System {
		t.Fatalf("the same input applied twice must render byte-identical system prompts:\nfirst = %q\nsecond = %q", first.System, second.System)
	}
	if !reflect.DeepEqual(first.Messages, second.Messages) {
		t.Fatalf("the same input applied twice must render identical messages:\nfirst = %#v\nsecond = %#v", first.Messages, second.Messages)
	}

	firstItems, secondItems := first.ContextManifest.Items, second.ContextManifest.Items
	if len(firstItems) != len(secondItems) {
		t.Fatalf("manifest item count must be deterministic: first=%d second=%d", len(firstItems), len(secondItems))
	}
	for i := range firstItems {
		if firstItems[i].ID != secondItems[i].ID || firstItems[i].Kind != secondItems[i].Kind {
			t.Fatalf("manifest item %d must be deterministic (no map-order leak): first=%+v second=%+v", i, firstItems[i], secondItems[i])
		}
	}
}
