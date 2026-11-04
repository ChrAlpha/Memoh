package contextview

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

func sizedMessageFrag(id string, msg sdk.Message, tokens int) contextfrag.ContextFrag {
	frag := messageFrag(id, msg)
	frag.TokenEstimate = tokens
	return frag
}

func selectProviderFrags(frags []contextfrag.ContextFrag, budget BudgetEnvelope) SelectionResult {
	selector := &FragmentSelector{}
	return selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), budget)
}

// Selection never trims history. Whatever the budget, every history
// fragment reaches the view: a request that does not fit is folded by an
// armed run or fails its rendered-envelope check, never cut down here.
func TestSelectionKeepsEveryHistoryFragmentWhateverTheBudget(t *testing.T) {
	t.Parallel()

	old := strings.Repeat("an old exchange that no longer fits ", 200)
	frags := []contextfrag.ContextFrag{
		sizedMessageFrag("old-user", sdk.UserMessage(old), 2000),
		sizedMessageFrag("old-assistant", sdk.AssistantMessage(old), 2000),
		toolCallFrag("old-call", "search", "call-1"),
		toolResultFrag("old-result", "search", "call-1", old),
		sizedMessageFrag("latest", sdk.UserMessage("latest"), 10),
	}
	selector := &FragmentSelector{}
	for _, intent := range []contextfrag.Intent{contextfrag.IntentRunConfigPreProvider, contextfrag.IntentDiscussReply} {
		for _, budgeted := range []bool{false, true} {
			var budget BudgetEnvelope
			if budgeted {
				plan, err := ComputeContextBudgetPlan(4*MinimumSystemBudgetTokens, MinimumSystemBudgetTokens, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				budget.Plan = plan
			}

			result := selector.Select(slices.Clone(frags), selector.ProfileFor(intent), budget)

			if result.FatalError != nil || len(result.Dropped) != 0 || len(result.Edited) != 0 {
				t.Fatalf("%s budgeted=%v: fatal=%v dropped=%#v edited=%#v", intent, budgeted, result.FatalError, fragIDs(result.Dropped), result.Edited)
			}
			assertSelectedIDs(t, result, fragIDs(frags))
		}
	}
}

// A tool result whose call is gone from the history is a guaranteed
// provider 400, so selection cuts it even when nothing else is dropped.
func TestOrphanToolResultIsCutFromOlderHistory(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		toolResultFrag("orphan-result", "search", "call-gone", "stale"),
		messageFrag("plain-old", sdk.UserMessage("plain history")),
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"plain-old", "latest"})
	assertDroppedReason(t, result, "orphan-result", orphanExchangeDropReason)
}

// The orphan cut reaches only older history: the turn in progress, from the
// latest user message on, is never cut.
func TestOrphanCutLeavesTheTurnInProgressAlone(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("plain-old", sdk.UserMessage("plain history")),
		messageFrag("latest", sdk.UserMessage("latest")),
		toolCallFrag("pending-call", "search", "call-1"),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"plain-old", "latest", "pending-call"})
}

// A turn that is the whole history, its request the first message, keeps
// its request and its latest step out of reach; an orphan in the steps
// between is cut like one in older history.
func TestOrphanCutReachesBetweenTheStepsOfALoneTurn(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("request", sdk.UserMessage("do the work")),
		toolCallFrag("orphan-call", "search", "call-1"),
		messageFrag("progress", sdk.AssistantMessage("still working")),
		toolCallFrag("latest-call", "search", "call-2"),
		toolResultFrag("latest-result", "search", "call-2", "found"),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"request", "progress", "latest-call", "latest-result"})
	assertDroppedReason(t, result, "orphan-call", orphanExchangeDropReason)
}

// An exchange with a protected member is kept whole, whatever it lacks: an
// ask_user exchange is never cut, and neither is a pinned fragment.
func TestOrphanExchangeWithAProtectedMemberIsKept(t *testing.T) {
	t.Parallel()

	pinned := toolResultFrag("pinned-result", "search", "call-gone", "kept")
	pinned.Budget.Overflow = contextfrag.OverflowKeep
	frags := []contextfrag.ContextFrag{
		toolCallFrag("ask-call", "ask_user", "call-ask"),
		pinned,
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"ask-call", "pinned-result", "latest"})
}

// Pairing follows appearance order: a call opens its tool_call_id, the next
// result seen for that id closes it, and the id is then free for a later
// call to reopen. Backends that emit deterministic per-turn ids
// (llama.cpp/vLLM style "call_0") reuse one id across unrelated exchanges,
// so a later call left without its result is cut alone while the complete
// exchange before it, sharing its id, stays.
func TestReusedToolCallIDKeepsExchangesIndependent(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		toolCallFrag("old-call", "search", "call_0"),
		toolResultFrag("old-result", "search", "call_0", "found"),
		toolCallFrag("new-call", "search", "call_0"),
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"old-call", "old-result", "latest"})
	assertDroppedReason(t, result, "new-call", orphanExchangeDropReason)
}

// A protected exchange reusing an orphan's tool_call_id does not shelter
// it: pairing by id alone would fold both into one protected unit and send
// the orphan to the provider.
func TestReusedToolCallIDOrphanIsCutDespiteAProtectedReuse(t *testing.T) {
	t.Parallel()

	newCall := toolCallFrag("new-call", "search", "call_0")
	newCall.Budget.Overflow = contextfrag.OverflowKeep
	frags := []contextfrag.ContextFrag{
		toolCallFrag("old-call", "search", "call_0"),
		newCall,
		toolResultFrag("new-result", "search", "call_0", "found"),
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"new-call", "new-result", "latest"})
	assertDroppedReason(t, result, "old-call", orphanExchangeDropReason)
}

// A result seen before any call for its id closes nothing, so it is an
// orphan, and the call that later opens the same id is never closed, so it
// is one too. Real backends reuse ids strictly call-then-result; pairing the
// two regardless of order would only ever keep a genuine orphan.
func TestResultBeforeCallBothOrphan(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		messageFrag("plain-old", sdk.UserMessage("plain history")),
		toolResultFrag("early-result", "search", "call-1", "found"),
		toolCallFrag("later-call", "search", "call-1"),
		messageFrag("latest", sdk.UserMessage("latest")),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{})

	assertSelectedIDs(t, result, []string{"plain-old", "latest"})
	assertDroppedReason(t, result, "early-result", orphanExchangeDropReason)
	assertDroppedReason(t, result, "later-call", orphanExchangeDropReason)
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
