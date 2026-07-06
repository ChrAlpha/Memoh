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
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot old ask"), 150, contextfrag.AttentionMention),
		attentionMessageFrag("passive-recent", sdk.UserMessage("recent group chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 200, RecentProtectTokens: 100})

	assertSelectedIDs(t, result, []string{"passive-recent", "latest"})
	assertDroppedReason(t, result, "directed-old", budgetDropReasonDirected)
}

// When the budget is too small to hold the recent-protect window, the window
// yields in the same tier order (passive first) instead of shielding recent
// passives at the expense of older directed traffic, and the drops honestly
// read budget:recent_window.
func TestBudgetAttention_WindowYieldsTierOrderedUnderSmallBudget(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot old ask"), 100, contextfrag.AttentionMention),
		attentionMessageFrag("passive-new", sdk.UserMessage("recent group chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150, RecentProtectTokens: 1000})

	assertSelectedIDs(t, result, []string{"directed-old", "latest"})
	assertDroppedReason(t, result, "passive-new", budgetDropReasonRecentWindow)
}

// The whole span sits inside the window here, so drops walk the in-window
// band tier by tier (passive, then directed, oldest first) under the
// budget:recent_window reason; raising the budget stops earlier along the
// same order and monotonically keeps more.
func TestBudgetAttention_MoreBudgetNeverDropsMore(t *testing.T) {
	t.Parallel()

	buildFrags := func() []contextfrag.ContextFrag {
		return []contextfrag.ContextFrag{
			attentionMessageFrag("window-1", sdk.UserMessage("one"), 100, contextfrag.AttentionMention),
			attentionMessageFrag("window-2", sdk.UserMessage("two"), 100, contextfrag.AttentionPassive),
			attentionMessageFrag("window-3", sdk.UserMessage("three"), 100, contextfrag.AttentionMention),
			attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
		}
	}

	tight := selectProviderFrags(buildFrags(), BudgetEnvelope{MaxTokens: 150, RecentProtectTokens: 1000})
	assertSelectedIDs(t, tight, []string{"window-3", "latest"})
	assertDroppedReason(t, tight, "window-2", budgetDropReasonRecentWindow)
	assertDroppedReason(t, tight, "window-1", budgetDropReasonRecentWindow)

	mid := selectProviderFrags(buildFrags(), BudgetEnvelope{MaxTokens: 250, RecentProtectTokens: 1000})
	assertSelectedIDs(t, mid, []string{"window-1", "window-3", "latest"})
	assertDroppedReason(t, mid, "window-2", budgetDropReasonRecentWindow)

	loose := selectProviderFrags(buildFrags(), BudgetEnvelope{MaxTokens: 400, RecentProtectTokens: 1000})
	assertSelectedIDs(t, loose, []string{"window-1", "window-2", "window-3", "latest"})
}

func keptIDSet(result SelectionResult) map[string]bool {
	kept := make(map[string]bool, len(result.Selected))
	for _, frag := range result.Selected {
		kept[frag.ID] = true
	}
	return kept
}

// windowShiftFrags is the review scenario where a budget-scaled window broke
// cross-budget monotonicity: the larger budget widened the window, protected
// more recent passives out of the passive band, and pushed pressure into the
// older untiered band — dropping the 95-token message the smaller budget kept.
func windowShiftFrags() []contextfrag.ContextFrag {
	return []contextfrag.ContextFrag{
		attentionMessageFrag("untiered-old", sdk.UserMessage("plain history"), 95),
		attentionMessageFrag("passive-a", sdk.UserMessage("chatter a"), 20, contextfrag.AttentionPassive),
		attentionMessageFrag("passive-b", sdk.UserMessage("chatter b"), 30, contextfrag.AttentionPassive),
		attentionMessageFrag("passive-c", sdk.UserMessage("chatter c"), 60, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 50, contextfrag.AttentionDirect),
	}
}

// The drop order is a fixed total order independent of the budget; the budget
// only decides how far along it the drops go. Raising the budget must
// therefore never drop a fragment the smaller budget kept.
func TestBudgetAttention_LargerBudgetKeepsSuperset(t *testing.T) {
	t.Parallel()

	small := selectProviderFrags(windowShiftFrags(), BudgetEnvelope{MaxTokens: 100, RecentProtectTokens: 100})
	large := selectProviderFrags(windowShiftFrags(), BudgetEnvelope{MaxTokens: 180, RecentProtectTokens: 100})

	largeKept := keptIDSet(large)
	for _, frag := range small.Selected {
		if !largeKept[frag.ID] {
			t.Errorf("budget 180 dropped %q that budget 100 kept", frag.ID)
		}
	}
}

// Kept sets nest monotonically across any increasing budget ladder: shared
// banding (fixed recent-protect window, tier order, oldest-first) makes every
// larger budget stop earlier along the same drop sequence.
func TestBudgetAttention_KeptSetsNestAcrossBudgets(t *testing.T) {
	t.Parallel()

	budgets := []int{60, 100, 140, 180, 205}
	prev := selectProviderFrags(windowShiftFrags(), BudgetEnvelope{MaxTokens: budgets[0], RecentProtectTokens: 100})
	for _, budget := range budgets[1:] {
		next := selectProviderFrags(windowShiftFrags(), BudgetEnvelope{MaxTokens: budget, RecentProtectTokens: 100})
		nextKept := keptIDSet(next)
		for _, frag := range prev.Selected {
			if !nextKept[frag.ID] {
				t.Fatalf("budget %d dropped %q kept at a smaller budget", budget, frag.ID)
			}
		}
		prev = next
	}
}

// The trim notice may never split a kept tool closure. When the natural
// insertion point (after the last dropped fragment) falls between a kept
// call and its result, it slides to the closure's end.
func TestBudgetAttention_NoticeNeverSplitsKeptClosure(t *testing.T) {
	t.Parallel()

	call := toolCallFrag("directed-call", "search", "call-1")
	call.TokenEstimate = 50
	call.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionMention}
	callResult := toolResultFrag("directed-result", "search", "call-1", "found")
	callResult.TokenEstimate = 50

	frags := []contextfrag.ContextFrag{
		call,
		attentionMessageFrag("passive-mid", sdk.UserMessage("group chatter"), 100, contextfrag.AttentionPassive),
		callResult,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150})

	assertSelectedIDs(t, result, []string{"directed-call", "directed-result", "latest"})
	assertDroppedReason(t, result, "passive-mid", budgetDropReasonPassive)
	if !result.TrimNotice {
		t.Fatal("budget drop must raise a trim notice")
	}
	if result.TrimNoticeIndex != 2 {
		t.Fatalf("TrimNoticeIndex = %d, want 2 (after the kept closure)", result.TrimNoticeIndex)
	}
}

// The notice wording is honest about scattered trimming — holes can be
// mid-history, not only at the head. Model-visible change locked here.
func TestHistoryTrimNoticeWordingHonestAboutHoles(t *testing.T) {
	t.Parallel()

	want := "[System Notice] Some earlier and intervening messages were trimmed to fit the context window. " +
		"If you need information from the trimmed messages, use the available tools " +
		"(such as memory_read or web search) to retrieve it."
	if HistoryTrimNotice != want {
		t.Fatalf("HistoryTrimNotice = %q, want %q", HistoryTrimNotice, want)
	}
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

// A tool closure with mixed droppability (one member pinned, the other
// droppable) must be kept whole; dropping only the droppable half leaves an
// orphan tool_use or tool_result the provider rejects with a 400.
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

// A closure's tier comes from its attention-bearing members only. A passive
// call whose tool result carries no attention data stays in the passive band
// instead of being promoted to untiered.
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

// A droppable tool result whose call is absent from the set is a guaranteed
// provider 400; with a budget in force it drops unconditionally, restoring
// the legacy leading-orphan cut even when everything fits.
//
// An orphan drop is not a space trim: with nothing else under pressure, it
// must not raise the trim notice either.
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
	assertDroppedReason(t, result, "orphan-result", budgetDropReasonOrphanExchange)
	if result.TrimNotice {
		t.Fatal("an orphan-only drop is not a space trim and must not raise a trim notice")
	}
}

// A genuine space trim alongside an orphan drop must still raise the notice:
// only the orphan-only case is silent.
func TestBudgetAttention_OrphanPlusSpatialDropRaisesTrimNotice(t *testing.T) {
	t.Parallel()

	orphan := toolResultFrag("orphan-result", "search", "call-gone", "stale")
	orphan.TokenEstimate = 10

	frags := []contextfrag.ContextFrag{
		orphan,
		attentionMessageFrag("passive-old", sdk.UserMessage("chatter"), 100, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 50})

	assertSelectedIDs(t, result, []string{"latest"})
	assertDroppedReason(t, result, "orphan-result", budgetDropReasonOrphanExchange)
	assertDroppedReason(t, result, "passive-old", budgetDropReasonPassive)
	if !result.TrimNotice {
		t.Fatal("a genuine space trim alongside an orphan drop must still raise the trim notice")
	}
}

// buildBudgetUnits scopes tool-call pairing to appearance order: a call
// opens its tool_call_id, the next result seen for that id closes it, and
// the id is then free for a later call to reopen. Backends that emit
// deterministic per-turn ids (llama.cpp/vLLM style "call_0") reuse the same
// id across unrelated exchanges; without appearance-order scoping, reusing
// an id collapses two independent exchanges into one atomic drop unit. Here
// the budget only needs the older exchange gone — the newer, smaller reuse
// of the same id must survive on its own instead of being dragged along.
func TestBudgetAttention_ReusedToolCallIDKeepsExchangesIndependent(t *testing.T) {
	t.Parallel()

	oldCall := toolCallFrag("old-call", "search", "call_0")
	oldCall.TokenEstimate = 50
	oldCall.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}
	oldResult := toolResultFrag("old-result", "search", "call_0", "found")
	oldResult.TokenEstimate = 50

	newCall := toolCallFrag("new-call", "search", "call_0")
	newCall.TokenEstimate = 25
	newCall.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}
	newResult := toolResultFrag("new-result", "search", "call_0", "found")
	newResult.TokenEstimate = 25

	frags := []contextfrag.ContextFrag{
		oldCall,
		oldResult,
		newCall,
		newResult,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 50})

	assertSelectedIDs(t, result, []string{"new-call", "new-result", "latest"})
	assertDroppedReason(t, result, "old-call", budgetDropReasonPassive)
	assertDroppedReason(t, result, "old-result", budgetDropReasonPassive)
}

// A must-keep marker on a newer reused-id exchange must not contaminate an
// unrelated older exchange sharing the same tool_call_id: id-keyed merging
// (rather than appearance-order scoping) would fold both into one unit,
// have the must-keep tag make the whole thing undroppable, and blow through
// the budget with the older exchange still attached — a guaranteed 400 that
// nothing in the older exchange itself justifies.
func TestBudgetAttention_ReusedToolCallIDOldExchangeDropsDespiteNewMustKeep(t *testing.T) {
	t.Parallel()

	oldCall := toolCallFrag("old-call", "search", "call_0")
	oldCall.TokenEstimate = 5000
	oldCall.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}
	oldResult := toolResultFrag("old-result", "search", "call_0", "found")
	oldResult.TokenEstimate = 5000

	newCall := toolCallFrag("new-call", "search", "call_0")
	newCall.TokenEstimate = 50
	newCall.Budget.Overflow = contextfrag.OverflowKeep
	newResult := toolResultFrag("new-result", "search", "call_0", "found")
	newResult.TokenEstimate = 50

	frags := []contextfrag.ContextFrag{
		oldCall,
		oldResult,
		newCall,
		newResult,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 200})

	assertSelectedIDs(t, result, []string{"new-call", "new-result", "latest"})
	assertDroppedReason(t, result, "old-call", budgetDropReasonPassive)
	assertDroppedReason(t, result, "old-result", budgetDropReasonPassive)
}

// A result observed before any call for its id has no open unit to close —
// it is itself an orphan — and the call that later opens the same id is
// never closed by anything after it, so it is an orphan too. Appearance-
// order scoping drops both unconditionally instead of the old union-find
// behavior that paired them regardless of order: result-before-call for the
// same id is not a shape real backends produce (ids are reused strictly
// call-then-result), so pairing it was solving a problem this design
// doesn't have, at the cost of ever dropping a genuine orphan wrong.
func TestBudgetAttention_ResultBeforeCallBothOrphan(t *testing.T) {
	t.Parallel()

	early := toolResultFrag("early-result", "search", "call-1", "found")
	early.TokenEstimate = 40
	later := toolCallFrag("later-call", "search", "call-1")
	later.TokenEstimate = 40
	later.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionPassive}

	frags := []contextfrag.ContextFrag{
		attentionMessageFrag("directed-old", sdk.UserMessage("@bot keep this"), 100, contextfrag.AttentionMention),
		early,
		later,
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 1000})

	assertSelectedIDs(t, result, []string{"directed-old", "latest"})
	assertDroppedReason(t, result, "early-result", budgetDropReasonOrphanExchange)
	assertDroppedReason(t, result, "later-call", budgetDropReasonOrphanExchange)
	if result.TrimNotice {
		t.Fatal("orphan-only drops must not raise the trim notice")
	}
}

// Drops within a tier are contiguous oldest-first; priority no longer
// reorders them, so a newer zero-estimate unit is not sacrificed for zero
// gain before an older unit that actually frees tokens.
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
			attentionMessageFrag("directed-old", sdk.UserMessage("@bot old ask"), 250, contextfrag.AttentionMention),
			attentionMessageFrag("passive-new", sdk.UserMessage("recent group chatter"), 100, contextfrag.AttentionPassive),
			attentionMessageFrag("latest", sdk.UserMessage("latest"), 50, contextfrag.AttentionDirect),
		},
		ContextQueryMaterialized: true,
		ContextBudgetMaxTokens:   300,
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

// With no override the default 20K window applies; a budget far below it puts
// the whole history in-window, so drops follow tier order (passive first) and
// report budget:recent_window — the window's signature — instead of the
// windowless budget:passive.
func TestApplyProviderRunConfigDefaultsRecentProtectWindow(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	got := ApplyProviderRunConfig(context.Background(), nil, budgetAttentionRunConfig(holder))

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want directed history, notice, latest", len(got.Messages))
	}
	if text := messageText(t, got.Messages[0]); text != "@bot old ask" {
		t.Fatalf("first message = %q, want the directed message kept over in-window passive", text)
	}
	if text := messageText(t, got.Messages[1]); text != HistoryTrimNotice {
		t.Fatalf("second message = %q, want trim notice at the drop point", text)
	}
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder has no snapshot")
	}
	if snapshot.Selection.DropReasons[budgetDropReasonRecentWindow] != 1 {
		t.Fatalf("drop reasons = %#v, want one in-window drop under the default window", snapshot.Selection.DropReasons)
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

	orphan := toolResultFrag("orphan-result", "search", "call-gone", "stale")
	orphan.TokenEstimate = 10

	frags := []contextfrag.ContextFrag{
		orphan,
		attentionMessageFrag("passive-1", sdk.UserMessage("chatter"), 50, contextfrag.AttentionPassive),
		attentionMessageFrag("untiered-1", sdk.UserMessage("plain"), 50),
		attentionMessageFrag("directed-1", sdk.UserMessage("@bot old"), 200, contextfrag.AttentionMention),
		attentionMessageFrag("passive-recent", sdk.UserMessage("recent chatter"), 50, contextfrag.AttentionPassive),
		attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
	}

	result := selectProviderFrags(frags, BudgetEnvelope{MaxTokens: 150, RecentProtectTokens: 200})

	assertSelectedIDs(t, result, []string{"passive-recent", "latest"})
	histogram := dropReasonHistogram(result.Summary.DropReasons)
	want := map[string]int{
		budgetDropReasonOrphanExchange: 1,
		budgetDropReasonPassive:        1,
		budgetDropReasonUntiered:       1,
		budgetDropReasonDirected:       1,
	}
	for reason, count := range want {
		if histogram[reason] != count {
			t.Fatalf("histogram[%s] = %d, want %d; full histogram %#v", reason, histogram[reason], count, histogram)
		}
	}
}

// End to end, chat-path history drops read budget:untiered in the lifecycle
// histogram; the request's own attention no longer colors history fragments
// that have no per-message attention data.
func TestApplyProviderRunConfigChatHistoryPressureReportsUntiered(t *testing.T) {
	t.Parallel()

	holder := contextfrag.NewLifecycleHolder()
	zero := 0
	got := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
		Messages: []sdk.Message{
			sdk.UserMessage("old question"),
			sdk.AssistantMessage("old reply"),
			sdk.UserMessage("current question"),
		},
		ContextHistoryTokenEstimates: []int{100, 5, 5},
		ContextTrimmableMessages:     3,
		ContextBudgetMaxTokens:       50,
		ContextRecentProtectTokens:   &zero,
		ContextQueryMaterialized:     true,
		ContextScope: contextfrag.Scope{
			BotID:     "bot-1",
			SessionID: "s1",
			Attention: []contextfrag.AttentionReason{contextfrag.AttentionDirect},
		},
		ContextLifecycle: holder,
	})

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want notice plus two kept", len(got.Messages))
	}
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder has no snapshot")
	}
	if snapshot.Selection.DropReasons[budgetDropReasonUntiered] != 1 {
		t.Fatalf("drop reasons = %#v, want one untiered history drop", snapshot.Selection.DropReasons)
	}
	if snapshot.Selection.DropReasons[budgetDropReasonDirected] != 0 {
		t.Fatalf("drop reasons = %#v, want no directed drops from request attention", snapshot.Selection.DropReasons)
	}
}

// End to end, the rendered provider stream keeps a tool call adjacent to its
// result with the trim notice after the closure, never inside.
func TestApplyProviderRunConfigNoticeSlidesPastKeptClosure(t *testing.T) {
	t.Parallel()

	call := toolCallFrag("directed-call", "search", "call-1")
	call.TokenEstimate = 50
	call.Scope.Attention = []contextfrag.AttentionReason{contextfrag.AttentionMention}
	callResult := toolResultFrag("directed-result", "search", "call-1", "found")
	callResult.TokenEstimate = 50

	got := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
		ContextSourceFrags: []contextfrag.ContextFrag{
			call,
			attentionMessageFrag("passive-mid", sdk.UserMessage("group chatter"), 100, contextfrag.AttentionPassive),
			callResult,
			attentionMessageFrag("latest", sdk.UserMessage("latest"), 100, contextfrag.AttentionDirect),
		},
		ContextQueryMaterialized: true,
		ContextBudgetMaxTokens:   150,
		ContextScope:             contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
	})

	if len(got.Messages) != 4 {
		t.Fatalf("messages = %d, want call, result, notice, latest", len(got.Messages))
	}
	if _, ok := got.Messages[0].Content[0].(sdk.ToolCallPart); !ok {
		t.Fatalf("messages[0] = %#v, want the kept tool call", got.Messages[0])
	}
	if got.Messages[1].Role != sdk.MessageRoleTool {
		t.Fatalf("messages[1] role = %q, want the tool result adjacent to its call", got.Messages[1].Role)
	}
	if text := messageText(t, got.Messages[2]); text != HistoryTrimNotice {
		t.Fatalf("messages[2] = %q, want the trim notice after the closure", text)
	}
	if text := messageText(t, got.Messages[3]); text != "latest" {
		t.Fatalf("messages[3] = %q, want the latest user message", text)
	}
}

// End to end, a mixed-droppability closure survives budget pressure whole,
// so the rendered stream never carries an orphan tool call or orphan tool
// result.
func TestApplyProviderRunConfigMixedClosureStaysWhole(t *testing.T) {
	t.Parallel()

	pinnedCall := toolCallFrag("pinned-call", "search", "call-1")
	pinnedCall.TokenEstimate = 50
	pinnedCall.Budget.Overflow = contextfrag.OverflowKeep
	droppableResult := toolResultFrag("droppable-result", "search", "call-1", "found")
	droppableResult.TokenEstimate = 200

	got := ApplyProviderRunConfig(context.Background(), nil, agentpkg.RunConfig{
		ContextSourceFrags: []contextfrag.ContextFrag{
			attentionMessageFrag("filler-old", sdk.UserMessage("old filler"), 100),
			pinnedCall,
			droppableResult,
			attentionMessageFrag("latest", sdk.UserMessage("latest"), 50, contextfrag.AttentionDirect),
		},
		ContextQueryMaterialized: true,
		ContextBudgetMaxTokens:   50,
		ContextScope:             contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
	})

	assertNoOrphanToolExchange(t, got.Messages)
	if len(got.Messages) != 4 {
		t.Fatalf("messages = %d, want notice, call, result, latest", len(got.Messages))
	}
	if text := messageText(t, got.Messages[0]); text != HistoryTrimNotice {
		t.Fatalf("messages[0] = %q, want trim notice", text)
	}
}

// assertNoOrphanToolExchange fails when any tool call is not immediately
// followed by a tool message answering it, or a tool message lacks a matching
// pending call — both are provider 400s.
func assertNoOrphanToolExchange(t *testing.T, messages []sdk.Message) {
	t.Helper()
	pending := map[string]bool{}
	for i, msg := range messages {
		if msg.Role == sdk.MessageRoleTool {
			for _, part := range msg.Content {
				result, ok := part.(sdk.ToolResultPart)
				if !ok {
					continue
				}
				if !pending[result.ToolCallID] {
					t.Fatalf("messages[%d]: orphan tool result %q", i, result.ToolCallID)
				}
				delete(pending, result.ToolCallID)
			}
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("messages[%d]: tool calls %v not followed by tool results", i, pending)
		}
		for _, part := range msg.Content {
			if call, ok := part.(sdk.ToolCallPart); ok {
				pending[call.ToolCallID] = true
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("unanswered tool calls at end of stream: %v", pending)
	}
}
