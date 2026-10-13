package contextview

import (
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func attentionMessageFrag(id string, msg sdk.Message, tokens int, reasons ...contextfrag.AttentionReason) contextfrag.ContextFrag {
	frag := messageFrag(id, msg)
	frag.TokenEstimate = tokens
	frag.Scope.Attention = reasons
	return frag
}

func selectProviderFrags(frags []contextfrag.ContextFrag, budget BudgetEnvelope) SelectionResult {
	selector := &FragmentSelector{}
	return selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), budget)
}

// Budget pressure drops nothing: every closed exchange and every message,
// passive or directed, reaches the provider.
func TestSelectorOverBudgetKeepsHistoryAndClosedExchanges(t *testing.T) {
	t.Parallel()

	call := toolCallFrag("call", "search", "call-1")
	call.TokenEstimate = 500
	callResult := toolResultFrag("result", "search", "call-1", "found")
	callResult.TokenEstimate = 500
	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("passive-old", sdk.UserMessage("group chatter"), 100, contextfrag.AttentionPassive),
		call,
		callResult,
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot do it"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 50})

	assertSelectedIDs(t, result, []string{"passive-old", "call", "result", "directed-old", "latest"})
	if len(result.Dropped) != 0 {
		t.Fatalf("dropped = %#v, want none", fragIDs(result.Dropped))
	}
}

// A droppable tool result whose call is absent from the set is a guaranteed
// provider 400; with a budget in force it drops even when everything fits.
func TestSelectorDropsOrphanToolResult(t *testing.T) {
	t.Parallel()

	orphan := toolResultFrag("orphan-result", "search", "call-gone", "stale")
	orphan.TokenEstimate = 10

	frags := []contextfrag.ContextFrag{
		orphan,
		attentionMessageFrag("plain-old", sdk.UserMessage("plain history"), 100),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 1000})

	assertSelectedIDs(t, result, []string{"plain-old", "latest"})
	assertDroppedReason(t, result, "orphan-result", budgetDropReasonOrphanExchange)
}

// A protected orphan is kept: only droppable fragments are repaired.
func TestSelectorKeepsProtectedOrphanToolCall(t *testing.T) {
	t.Parallel()

	pinned := toolCallFrag("pinned-call", "search", "call-1")
	pinned.Budget.Overflow = contextfrag.OverflowKeep

	frags := []contextfrag.ContextFrag{
		pinned,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 1000})

	assertSelectedIDs(t, result, []string{"pinned-call", "latest"})
}

// buildBudgetUnits scopes tool-call pairing to appearance order: a call
// opens its tool_call_id, the next result seen for that id closes it, and
// the id is then free for a later call to reopen. Backends that emit
// deterministic per-turn ids (llama.cpp/vLLM style "call_0") reuse the same
// id across unrelated exchanges. An unanswered call followed by a complete
// exchange reusing its id is one orphan and one closed pair, not one unit.
func TestSelectorReusedToolCallIDKeepsExchangesIndependent(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		toolCallFrag("old-call", "search", "call_0"),
		toolCallFrag("new-call", "search", "call_0"),
		toolResultFrag("new-result", "search", "call_0", "found"),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 1000})

	assertSelectedIDs(t, result, []string{"new-call", "new-result", "latest"})
	assertDroppedReason(t, result, "old-call", budgetDropReasonOrphanExchange)
}

// A result observed before any call for its id has no open unit to close, so
// it is an orphan; the call that later opens the same id is never closed, so
// it is an orphan too. Both drop.
func TestSelectorResultBeforeCallBothOrphan(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot keep this"), 100, contextfrag.AttentionMention),
		toolResultFrag("early-result", "search", "call-1", "found"),
		toolCallFrag("later-call", "search", "call-1"),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 1000})

	assertSelectedIDs(t, result, []string{"directed-old", "latest"})
	assertDroppedReason(t, result, "early-result", budgetDropReasonOrphanExchange)
	assertDroppedReason(t, result, "later-call", budgetDropReasonOrphanExchange)
}

func assertSelectedIDs(t *testing.T, result SelectionResult, want []string) {
	t.Helper()
	got := fragIDs(result.Selected)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected ids = %#v, want %#v; dropped=%#v", got, want, fragIDs(result.Dropped))
	}
	if result.Summary.TotalSelected != len(want) {
		t.Fatalf("TotalSelected = %d, want %d", result.Summary.TotalSelected, len(want))
	}
}

func assertDroppedReason(t *testing.T, result SelectionResult, fragID, reason string) {
	t.Helper()
	for _, record := range result.Summary.DropReasons {
		if record.FragID == fragID {
			if record.Reason != reason {
				t.Fatalf("drop reason for %s = %q, want %q", fragID, record.Reason, reason)
			}
			return
		}
	}
	t.Fatalf("missing drop record for %s; records=%#v", fragID, result.Summary.DropReasons)
}

func toolCallFrag(id, name, callID string) contextfrag.ContextFrag {
	return messageFrag(id, sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ToolCallPart{ToolCallID: callID, ToolName: name, Input: toolexec.ArgumentsFromValue(map[string]any{})},
		},
	})
}

func toolResultFrag(id, name, callID, result string) contextfrag.ContextFrag {
	return messageFrag(id, sdk.ToolMessage(sdk.ToolResultPart{
		ToolCallID: callID,
		ToolName:   name,
		Result:     toolexec.OutputFromValue(result),
	}))
}
