package contextview

import (
	"context"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
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

func TestBudgetAttention_NoPressureKeepsEverything(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("passive-old", sdk.UserMessage("group chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot do it"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 1000, RecentProtectTokens: 50})

	assertSelectedIDs(t, result, []string{"passive-old", "directed-old", "latest"})
	if result.TrimNotice {
		t.Fatal("no budget pressure must not raise a trim notice")
	}
}

func TestBudgetAttention_PassiveDropsBeforeDirected(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-1", sdk.UserMessage("@bot summarize"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("passive-1", sdk.UserMessage("group chatter one"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("directed-2", sdk.UserMessage("replying to bot"), 100, contextfrag.AttentionReply),
		attentionMessageFrag("passive-2", sdk.UserMessage("group chatter two"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 200})

	assertSelectedIDs(t, result, []string{"directed-1", "directed-2", "latest"})
	assertDroppedReason(t, result, "passive-1", budgetDropReasonPassive)
	assertDroppedReason(t, result, "passive-2", budgetDropReasonPassive)
}

func TestBudgetAttention_UntieredDropsAfterPassiveBeforeDirected(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-1", sdk.UserMessage("@bot look"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("untiered-1", sdk.UserMessage("plain chat history"), 100),
		attentionMessageFrag("passive-1", sdk.UserMessage("group chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 100})

	assertSelectedIDs(t, result, []string{"directed-1", "latest"})
	assertDroppedReason(t, result, "passive-1", budgetDropReasonPassive)
	assertDroppedReason(t, result, "untiered-1", budgetDropReasonUntiered)
}

func TestBudgetAttention_UntieredOnlyKeepsOldestFirstOrder(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("untiered-1", sdk.UserMessage("first"), 100),
		attentionMessageFrag("untiered-2", sdk.UserMessage("second"), 100),
		attentionMessageFrag("untiered-3", sdk.UserMessage("third"), 100),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 200})

	assertSelectedIDs(t, result, []string{"untiered-2", "untiered-3", "latest"})
	assertDroppedReason(t, result, "untiered-1", budgetDropReasonUntiered)
}

func TestBudgetAttention_RecentWindowProtectsPassive(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot old ask"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("passive-recent", sdk.UserMessage("recent group chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150, RecentProtectTokens: 100})

	assertSelectedIDs(t, result, []string{"passive-recent", "latest"})
	assertDroppedReason(t, result, "directed-old", budgetDropReasonDirected)
}

func TestBudgetAttention_WindowYieldsFromOldEndWhenOverBudget(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("window-1", sdk.UserMessage("one"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("window-2", sdk.UserMessage("two"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("window-3", sdk.UserMessage("three"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150, RecentProtectTokens: 1000})

	assertSelectedIDs(t, result, []string{"window-3", "latest"})
	assertDroppedReason(t, result, "window-1", budgetDropReasonWindowYield)
	assertDroppedReason(t, result, "window-2", budgetDropReasonWindowYield)
}

func TestBudgetAttention_ToolClosureDropsAtomically(t *testing.T) {
	t.Parallel()

	call := toolCallFrag("passive-call", "search", "call-1")
	call.TokenEstimate = 50
	call.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}
	callResult := toolResultFrag("passive-result", "search", "call-1", "found")
	callResult.TokenEstimate = 100
	callResult.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-1", sdk.UserMessage("@bot keep this"), 100, contextfrag.AttentionMention),
		call,
		callResult,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 100})

	assertSelectedIDs(t, result, []string{"directed-1", "latest"})
	assertDroppedReason(t, result, "passive-call", budgetDropReasonPassive)
	assertDroppedReason(t, result, "passive-result", budgetDropReasonPassive)
}

// Finding [1]: a tool closure with mixed droppability (one member pinned, the
// other droppable) must be kept whole; dropping only the droppable half leaves
// an orphan tool_use or tool_result the provider rejects with a 400.
func TestBudgetAttention_MixedDroppabilityClosureKeptWhole(t *testing.T) {
	t.Parallel()

	pinnedCall := toolCallFrag("pinned-call", "search", "call-1")
	pinnedCall.TokenEstimate = 50
	pinnedCall.Budget.Overflow = contextfrag.OverflowKeep
	droppableResult := toolResultFrag("droppable-result", "search", "call-1", "found")
	droppableResult.TokenEstimate = 200

	droppableCall := toolCallFrag("droppable-call", "search", "call-2")
	droppableCall.TokenEstimate = 200
	pinnedResult := toolResultFrag("pinned-result", "search", "call-2", "found")
	pinnedResult.TokenEstimate = 50
	pinnedResult.Budget.Overflow = contextfrag.OverflowKeep

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("filler-old", sdk.UserMessage("old filler"), 100),
		pinnedCall,
		droppableResult,
		droppableCall,
		pinnedResult,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 50})

	assertSelectedIDs(t, result, []string{"pinned-call", "droppable-result", "droppable-call", "pinned-result", "latest"})
	assertDroppedReason(t, result, "filler-old", budgetDropReasonUntiered)
}

// Finding [4]: a closure's tier comes from its attention-bearing members only.
// A passive call whose tool result carries no attention data stays in the
// passive band instead of being promoted to untiered.
func TestBudgetAttention_ClosureTierIgnoresAttentionlessMembers(t *testing.T) {
	t.Parallel()

	call := toolCallFrag("passive-call", "search", "call-1")
	call.TokenEstimate = 50
	call.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}
	callResult := toolResultFrag("plain-result", "search", "call-1", "found")
	callResult.TokenEstimate = 50

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("untiered-old", sdk.UserMessage("plain history"), 100),
		call,
		callResult,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150})

	assertSelectedIDs(t, result, []string{"untiered-old", "latest"})
	assertDroppedReason(t, result, "passive-call", budgetDropReasonPassive)
	assertDroppedReason(t, result, "plain-result", budgetDropReasonPassive)
}

// Finding [8]: a droppable tool result whose call is absent from the set is a
// guaranteed provider 400; with a budget in force it drops unconditionally,
// restoring the legacy leading-orphan cut even when everything fits.
func TestBudgetAttention_OrphanToolResultDroppedEvenUnderBudget(t *testing.T) {
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
	assertDroppedReason(t, result, "orphan-result", budgetDropReasonOrphanResult)
}

// Finding [9]: drops within a tier are contiguous oldest-first; priority no
// longer reorders them, so a newer zero-estimate unit is not sacrificed for
// zero gain before an older unit that actually frees tokens.
func TestBudgetAttention_OldestFirstWithinTierNoZeroGainScatter(t *testing.T) {
	t.Parallel()

	heavy := attentionMessageFrag("passive-heavy-old", sdk.UserMessage("very long chatter"), 100, contextfrag.AttentionPassive)
	heavy.Priority = 70
	zero := attentionMessageFrag("passive-zero-new", sdk.UserMessage("hi"), 0, contextfrag.AttentionPassive)
	zero.Priority = 10

	frags := []contextfrag.ContextFrag{
		heavy,
		zero,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 50})

	assertSelectedIDs(t, result, []string{"passive-zero-new", "latest"})
	assertDroppedReason(t, result, "passive-heavy-old", budgetDropReasonPassive)
}

func budgetAttentionRunConfig(holder *contextfrag.LifecycleHolder) agentpkg.RunConfig {
	return agentpkg.RunConfig{
		ContextSourceFrags: []contextfrag.ContextFrag{
			attentionMessageFrag("directed-old", sdk.UserMessage("@bot old ask"), 100, contextfrag.AttentionMention),
			attentionMessageFrag("passive-new", sdk.UserMessage("recent group chatter"), 100, contextfrag.AttentionPassive),
			attentionMessageFrag("latest", sdk.UserMessage("latest"), 50, contextfrag.AttentionDirect),
		},
		ContextQueryMaterialized: true,
		ContextBudgetMaxTokens:   150,
		ContextScope:             contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
		ContextLifecycle:         holder,
	}
}

func messageText(t *testing.T, msg sdk.Message) string {
	t.Helper()
	if len(msg.Content) == 0 {
		t.Fatalf("message has no content: %#v", msg)
	}
	text, ok := msg.Content[0].(sdk.TextPart)
	if !ok {
		t.Fatalf("message content = %#v, want text part", msg.Content[0])
	}
	return text.Text
}

func TestApplyProviderRunConfigDefaultsRecentProtectWindow(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	got := ApplyProviderRunConfig(context.Background(), nil, budgetAttentionRunConfig(holder))

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want notice plus two kept", len(got.Messages))
	}
	if text := messageText(t, got.Messages[0]); text != HistoryTrimNotice {
		t.Fatalf("first message = %q, want trim notice", text)
	}
	if text := messageText(t, got.Messages[1]); text != "recent group chatter" {
		t.Fatalf("kept message = %q, want the window-protected passive message", text)
	}
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder has no snapshot")
	}
	if snapshot.Selection.DropReasons[budgetDropReasonWindowYield] != 1 {
		t.Fatalf("drop reasons = %#v, want one recent window yield", snapshot.Selection.DropReasons)
	}
}

func TestApplyProviderRunConfigRecentProtectOverrideDisablesWindow(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	cfg := budgetAttentionRunConfig(holder)
	zero := 0
	cfg.ContextRecentProtectTokens = &zero

	got := ApplyProviderRunConfig(context.Background(), nil, cfg)

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want directed history, notice, latest", len(got.Messages))
	}
	if text := messageText(t, got.Messages[0]); text != "@bot old ask" {
		t.Fatalf("first message = %q, want the directed message kept", text)
	}
	if text := messageText(t, got.Messages[1]); text != HistoryTrimNotice {
		t.Fatalf("second message = %q, want trim notice after the drop point", text)
	}
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder has no snapshot")
	}
	if snapshot.Selection.DropReasons[budgetDropReasonPassive] != 1 {
		t.Fatalf("drop reasons = %#v, want one passive drop", snapshot.Selection.DropReasons)
	}
}

func TestBudgetAttention_DropReasonHistogram(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("passive-1", sdk.UserMessage("chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("untiered-1", sdk.UserMessage("plain"), 100),
		attentionMessageFrag("directed-1", sdk.UserMessage("@bot old"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("window-1", sdk.UserMessage("recent one"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("window-2", sdk.UserMessage("recent two"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150, RecentProtectTokens: 200})

	assertSelectedIDs(t, result, []string{"window-2", "latest"})
	histogram := dropReasonHistogram(result.Summary.DropReasons)
	want := map[string]int{
		budgetDropReasonWindowYield: 1,
		budgetDropReasonPassive:     1,
		budgetDropReasonUntiered:    1,
		budgetDropReasonDirected:    1,
	}
	for reason, count := range want {
		if histogram[reason] != count {
			t.Fatalf("histogram[%s] = %d, want %d; full histogram %#v", reason, histogram[reason], count, histogram)
		}
	}
}
