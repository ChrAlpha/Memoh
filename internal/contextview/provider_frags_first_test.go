package contextview

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func systemTextFrag(id, text string, kind contextfrag.Kind, priority int) contextfrag.ContextFrag {
	return contextfrag.TextFrag(contextfrag.TextFragInput{
		ID: id, Text: text, Kind: kind, Role: sdk.MessageRoleSystem, Slot: contextfrag.SlotSystem,
		Priority: priority, CacheClass: contextfrag.CacheStable, Trust: contextfrag.TrustSystem,
		Source: contextfrag.SourceRunConfig, Collector: "system_sections",
	})
}

func currentMessageFrag(id, text string) contextfrag.ContextFrag {
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID: id, Message: sdk.UserMessage(text), Kind: contextfrag.KindCurrentUserMessage,
		Slot: contextfrag.SlotCurrentUser, Priority: 90, CacheClass: contextfrag.CacheNever,
		Trust: contextfrag.TrustUser, Source: contextfrag.SourceRunConfig, Collector: currentUserCollectorName,
		Budget: contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})
}

func stableHistoryMessageFrag(id string, message sdk.Message) contextfrag.ContextFrag {
	frag := historyMessageFrag(id, message)
	frag.CacheClass = contextfrag.CacheStable
	return frag
}

func fragsFirstFixture() agentpkg.RunConfig {
	return agentpkg.RunConfig{
		System: "LEGACY_SYSTEM_IGNORED", Messages: []sdk.Message{sdk.UserMessage("LEGACY_MESSAGE_IGNORED")},
		ContextSourceFrags: []contextfrag.ContextFrag{
			systemTextFrag("system.prompt", "base system", contextfrag.KindSystemPrompt, 20),
			systemTextFrag("system.workspace", "## Workspace instruction files\n\nworkspace", contextfrag.KindWorkspaceInstruction, 50),
			stableHistoryMessageFrag("message.000", sdk.UserMessage("history")),
			currentMessageFrag("message.001", "current"),
		},
		ContextToolUsage: "## Tool usage\n\nUSE_TOOLS",
		ContextToolDefs:  []contextfrag.ToolDefAccounting{{Provider: "native", Name: "read", TokenEstimate: 5}},
	}
}

func TestApplyProviderRunConfigFragsFirst(t *testing.T) {
	t.Parallel()
	got := ApplyProviderRunConfig(context.Background(), nil, fragsFirstFixture())
	wantSystem := "base system\n\n## Tool usage\n\nUSE_TOOLS\n\n## Workspace instruction files\n\nworkspace"
	if got.System != wantSystem || strings.Contains(got.System, "LEGACY") {
		t.Fatalf("system = %q", got.System)
	}
	assertMessagesEqual(t, got.Messages, []sdk.Message{sdk.UserMessage("history"), sdk.UserMessage("current")})
	if len(got.ContextFrags) != 5 {
		t.Fatalf("frags = %#v", got.ContextFrags)
	}
	wantOrder := []string{"system.prompt", "system.tool_usage", "system.workspace", "message.000", "message.001"}
	for i, id := range wantOrder {
		if got.ContextFrags[i].ID != id {
			t.Fatalf("frag order = %#v", got.ContextFrags)
		}
	}
	wantEstimate := 5
	for _, frag := range got.ContextFrags[:4] {
		wantEstimate += contextfrag.ResolveFragTokens(frag)
	}
	if got.ContextCachePlan.StablePrefixTokenEstimate != wantEstimate || got.ContextCachePlan.StableMessageCount != 1 {
		t.Fatalf("cache plan = %#v, want estimate %d", got.ContextCachePlan, wantEstimate)
	}
}

func TestApplyProviderRunConfigDedupesToolUsage(t *testing.T) {
	t.Parallel()
	cfg := fragsFirstFixture()
	cfg.ContextSourceFrags = append(cfg.ContextSourceFrags, ToolUsageFrag("stale usage", cfg.ContextScope))
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if strings.Contains(got.System, "stale usage") || strings.Count(got.System, "## Tool usage") != 1 {
		t.Fatalf("system = %q", got.System)
	}
}

func TestCollectProviderSourceFragsSplitsWorkspaceAndPreservesMessageOrder(t *testing.T) {
	t.Parallel()
	index := 1
	cfg := agentpkg.RunConfig{
		System:                         "base\n\n## Workspace instruction files\n\nworkspace",
		Messages:                       []sdk.Message{sdk.AssistantMessage("before"), sdk.UserMessage("current"), sdk.AssistantMessage("after")},
		ContextCurrentUserMessageIndex: &index, ContextQueryMaterialized: true,
	}
	frags := CollectProviderSourceFrags(context.Background(), cfg)
	wantIDs := []string{"system.prompt", "system.workspace_instructions", "message.000", "message.001", "message.002"}
	if len(frags) != len(wantIDs) {
		t.Fatalf("frags = %#v", frags)
	}
	for i, id := range wantIDs {
		if frags[i].ID != id {
			t.Fatalf("frag ids = %#v", frags)
		}
	}
	got := ApplyProviderRunConfig(context.Background(), nil, cfg)
	if !reflect.DeepEqual(got.Messages, cfg.Messages) {
		t.Fatalf("messages = %#v, want %#v", got.Messages, cfg.Messages)
	}
}

func TestCollectNonSystemProviderSourceFragsExcludesSystem(t *testing.T) {
	t.Parallel()
	frags := CollectNonSystemProviderSourceFrags(context.Background(), agentpkg.RunConfig{
		System: "ignored", Messages: []sdk.Message{sdk.UserMessage("history")}, Query: "  current \n",
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
	})
	for _, frag := range frags {
		if frag.Slot == contextfrag.SlotSystem {
			t.Fatalf("system frag = %#v", frag)
		}
	}
	if len(frags) != 3 || frags[1].ID != "current_user.message" || frags[2].ID != "current_user.images" {
		t.Fatalf("frags = %#v", frags)
	}
}

func TestApplyProviderRunConfigDoesNotAddImplicitToolStripping(t *testing.T) {
	t.Parallel()
	for _, messageCount := range []int{10, 11} {
		messageCount := messageCount
		t.Run(fmt.Sprintf("messages_%d", messageCount), func(t *testing.T) {
			t.Parallel()
			messages := make([]sdk.Message, 0, messageCount)
			for i := 0; i < messageCount-2; i++ {
				messages = append(messages, sdk.UserMessage(fmt.Sprintf("history-%d", i)))
			}
			messages = append(messages,
				assistantToolCallMessage("call-1", "read", "checking"),
				toolResultMessage("call-1", "read", "exact tool result"),
			)
			cfg := agentpkg.RunConfig{System: "system", Messages: messages, ContextQueryMaterialized: true}
			cfg.ContextSourceFrags = CollectProviderSourceFrags(context.Background(), cfg)

			got := ApplyProviderRunConfig(context.Background(), nil, cfg)
			if !reflect.DeepEqual(got.Messages, messages) {
				t.Fatalf("nil tool policy changed %d messages: %#v", messageCount, got.Messages)
			}
		})
	}
}
