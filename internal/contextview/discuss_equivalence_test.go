package contextview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	pipeline "github.com/memohai/memoh/internal/chat/timeline"
)

func TestDiscussEquivalence_BasicRCAndTR(t *testing.T) {
	t.Parallel()

	assertDiscussEquivalent(t, discussLegacyInput{
		system: "Discuss system prompt.",
		rc: pipeline.RenderedContext{
			renderedTextSegment(100, "first rc"),
			renderedTextSegment(300, "second rc"),
		},
		trs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 200,
			Role:          "assistant",
			Content:       "assistant turn",
		}},
	})
}

func TestDiscussEquivalence_ConsecutiveRCMerged(t *testing.T) {
	t.Parallel()

	assertDiscussEquivalent(t, discussLegacyInput{
		rc: pipeline.RenderedContext{
			renderedTextSegment(100, "one"),
			renderedTextSegment(200, "two"),
			renderedTextSegment(300, "three"),
		},
	})
}

func TestDiscussEquivalence_CompactSummaryPrepended(t *testing.T) {
	t.Parallel()

	assertDiscussEquivalent(t, discussLegacyInput{
		summary: "older context summary",
		rc:      pipeline.RenderedContext{renderedTextSegment(100, "live context")},
	})
}

func TestDiscussEquivalence_TRWithRawContent(t *testing.T) {
	t.Parallel()

	assertDiscussEquivalent(t, discussLegacyInput{
		trs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 100,
			Role:          "tool",
			Content:       "debug text",
			RawContent:    json.RawMessage(`[{"type":"tool-result","toolCallId":"call-1","toolName":"lookup","result":{"answer":42}}]`),
		}},
	})
}

func TestDiscussEquivalence_EmptyInput(t *testing.T) {
	t.Parallel()

	assertDiscussEquivalent(t, discussLegacyInput{})
}

func TestDiscussEquivalence_RCBeforeTROnEqualTimestamp(t *testing.T) {
	t.Parallel()

	assertDiscussEquivalent(t, discussLegacyInput{
		rc: pipeline.RenderedContext{
			renderedTextSegment(100, "same-time rc"),
		},
		trs: []pipeline.TurnResponseEntry{{
			RequestedAtMs: 100,
			Role:          "assistant",
			Content:       "same-time tr",
		}},
	})
}

func TestDiscussSelector_ProfileMustKeepSystemAndCurrentUser(t *testing.T) {
	t.Parallel()

	profile := (&FragmentSelector{}).ProfileFor(contextfrag.IntentDiscussReply)

	if profile.Intent != contextfrag.IntentDiscussReply {
		t.Fatalf("Intent = %q, want %q", profile.Intent, contextfrag.IntentDiscussReply)
	}
	if !slotInProfile(profile, contextfrag.SlotSystem) {
		t.Fatalf("MustKeepSlots = %#v, want system", profile.MustKeepSlots)
	}
	if !slotInProfile(profile, contextfrag.SlotCurrentUser) {
		t.Fatalf("MustKeepSlots = %#v, want current_user", profile.MustKeepSlots)
	}
}

func TestDiscussSelector_BudgetedSelectionDropsCanDropHistory(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("old-assistant", sdk.AssistantMessage("old answer")),
		messageFrag("latest", sdk.UserMessage("latest question")),
	}
	selector := &FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentDiscussReply)

	result := selector.Select(frags, profile, BudgetEnvelope{MaxTokens: 1})

	assertSelectedIDs(t, result, []string{"latest"})
	assertDroppedReason(t, result, "old-user", budgetDropReasonUntiered)
	assertDroppedReason(t, result, "old-assistant", budgetDropReasonUntiered)
}

func TestDiscussSelector_BudgetedSelectionKeepsAllWhenWithinBudget(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("old-assistant", sdk.AssistantMessage("old answer")),
		messageFrag("latest", sdk.UserMessage("latest question")),
	}
	selector := &FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentDiscussReply)

	result := selector.Select(frags, profile, BudgetEnvelope{MaxTokens: 1000})

	assertSelectedIDs(t, result, []string{"old-user", "old-assistant", "latest"})
	if len(result.Dropped) != 0 {
		t.Fatalf("dropped = %#v, want none", fragIDs(result.Dropped))
	}
}

func TestSelector_ProviderBudgetedIntentDropsCanDropHistory(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("old-user", sdk.UserMessage("old question")),
		messageFrag("old-assistant", sdk.AssistantMessage("old answer")),
		messageFrag("latest", sdk.UserMessage("latest question")),
	}
	selector := &FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentRunConfigPreProvider)

	result := selector.Select(frags, profile, BudgetEnvelope{MaxTokens: 1})

	assertSelectedIDs(t, result, []string{"latest"})
	assertDroppedReason(t, result, "old-user", budgetDropReasonUntiered)
	assertDroppedReason(t, result, "old-assistant", budgetDropReasonUntiered)
}

type discussLegacyInput struct {
	system  string
	rc      pipeline.RenderedContext
	trs     []pipeline.TurnResponseEntry
	summary string
}

func assertDiscussEquivalent(t *testing.T, input discussLegacyInput) {
	t.Helper()
	scope := contextfrag.Scope{BotID: "bot-1", SessionID: "s1"}
	wantMessages := legacyDiscussMessages(input.rc, input.trs, input.summary)

	builder := NewBuilder(
		NewMapCollectorRegistry(
			&SystemPromptCollector{},
			&DiscussContextCollector{},
		),
		&FragmentSelector{},
		IdentityPlacer{},
		NewMapRendererRegistry(&SDKMessagesRenderer{}),
	)
	view, err := builder.Build(context.Background(), BuildInput{
		Scope:  scope,
		Intent: contextfrag.IntentDiscussReply,
		Sources: []SourceSpec{
			{Name: "system_prompt", Config: SystemPromptConfig{System: input.system}},
			{Name: "discuss_context", Config: DiscussContextConfig{
				RC:             input.rc,
				TRs:            input.trs,
				CompactSummary: input.summary,
			}},
		},
		Targets: []contextfrag.RenderTarget{contextfrag.RenderSDKMessages},
	})
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	rendered, ok := view.Rendered[contextfrag.RenderSDKMessages].Data.(*SDKRenderedPayload)
	if !ok {
		t.Fatalf("rendered data type = %T, want *SDKRenderedPayload", view.Rendered[contextfrag.RenderSDKMessages].Data)
	}

	if rendered.System != input.system {
		t.Fatalf("System = %q, want %q", rendered.System, input.system)
	}
	assertMessagesEqual(t, rendered.Messages, wantMessages)
}

func legacyDiscussMessages(rc pipeline.RenderedContext, trs []pipeline.TurnResponseEntry, summary string) []sdk.Message {
	var artifacts []pipeline.CompactionArtifact
	if strings.TrimSpace(summary) != "" {
		artifacts = []pipeline.CompactionArtifact{{ID: "equivalence-artifact", Summary: summary}}
	}
	composed := pipeline.ComposeContextWithArtifacts(rc, trs, artifacts)
	if composed == nil {
		return nil
	}
	return legacyContextMessagesToSDK(composed.Messages)
}

func legacyContextMessagesToSDK(messages []pipeline.ContextMessage) []sdk.Message {
	result := make([]sdk.Message, 0, len(messages))
	for _, m := range messages {
		if len(m.RawContent) > 0 {
			raw, err := json.Marshal(struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}{
				Role:    m.Role,
				Content: m.RawContent,
			})
			if err == nil {
				var msg sdk.Message
				if json.Unmarshal(raw, &msg) == nil {
					result = append(result, msg)
					continue
				}
			}
		}
		switch m.Role {
		case "user":
			result = append(result, sdk.UserMessage(m.Content))
		case "assistant":
			result = append(result, sdk.AssistantMessage(m.Content))
		case "tool":
			result = append(result, sdk.UserMessage(m.Content))
		default:
			result = append(result, sdk.UserMessage(m.Content))
		}
	}
	return result
}

func TestDiscussSDKContextBuilderMatchesLegacy(t *testing.T) {
	t.Parallel()

	rc := pipeline.RenderedContext{
		renderedTextSegment(100, "first rc"),
		renderedTextSegment(300, "second rc"),
	}
	trs := []pipeline.TurnResponseEntry{{
		RequestedAtMs: 200,
		Role:          "assistant",
		Content:       "assistant turn",
	}}

	builder := &DiscussSDKContextBuilder{}
	got, err := builder.BuildDiscussSDKMessages(context.Background(), contextfrag.Scope{BotID: "bot-1"}, DiscussContextInput{RC: rc, TRs: trs, CompactSummary: "older summary"})
	if err != nil {
		t.Fatalf("BuildDiscussSDKMessages failed: %v", err)
	}

	want := legacyDiscussMessages(rc, trs, "older summary")
	assertMessagesEqual(t, got, want)
}
