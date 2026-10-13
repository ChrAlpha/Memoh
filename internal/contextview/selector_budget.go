package contextview

import (
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// budgetDropReasonOrphanExchange names a fragment dropped because it carries
// only one side of a tool exchange.
const budgetDropReasonOrphanExchange = "budget:orphan_tool_exchange"

// budgetUnit is the atomic drop unit: a lone fragment, or an assistant
// tool-call fragment grouped with its tool-result fragments so a drop never
// breaks a tool closure. Units span the whole tagged set: when any member is
// protected the entire unit is (mixed droppability would orphan half a
// closure).
type budgetUnit struct {
	indexes   []int
	droppable bool
	hasCall   bool
	hasResult bool
}

// orphanToolExchange reports a unit that carries only one side of a tool
// exchange: a call nothing ever answers, or a result answering a call that
// is not (or no longer) in the set. Either shape is a guaranteed provider
// 400 if it reaches the wire, so both drop unconditionally.
func (u *budgetUnit) orphanToolExchange() bool {
	return u.hasCall != u.hasResult
}

// buildBudgetUnits pairs tool calls with their results in a single forward
// pass keyed by tool_call_id and scoped to appearance order, not id identity
// alone: a call opens its id, the next result seen for that id closes it,
// and the id is then free for a later call to reopen. Backends that emit
// deterministic per-turn ids (llama.cpp/vLLM style "call_0") reuse the same
// id across unrelated exchanges; scoping by the currently-open call keeps
// each exchange its own drop unit instead of collapsing every reuse of an id
// into one atomic (and possibly permanently undroppable) unit.
//
// A call whose id is already open when a new call for that id arrives does
// not close the earlier one: the earlier call stays open — and therefore
// orphaned, since nothing will ever close it — while the new call starts its
// own unit. A result whose id has no open call is likewise an orphan. This
// symmetric treatment also covers a result observed before its matching
// call: the result finds no open call (orphan), and the call that follows
// then opens a unit nothing ever closes (orphan too) — both drop, instead of
// the old union-find behavior that paired them regardless of order.
func buildBudgetUnits(tagged []TaggedFrag) []budgetUnit {
	units := make([]budgetUnit, 0, len(tagged))
	open := make(map[string]int)

	for i, taggedFrag := range tagged {
		frag := taggedFrag.Frag
		callIDs := fragToolCallIDs(frag)
		resultIDs := fragToolResultCallIDs(frag)

		home := -1
		for _, id := range resultIDs {
			idx, ok := open[id]
			if !ok {
				continue
			}
			delete(open, id)
			if home == -1 {
				home = idx
				continue
			}
			if home != idx {
				mergeBudgetUnits(units, home, idx, open)
			}
		}
		if home == -1 {
			units = append(units, budgetUnit{droppable: true})
			home = len(units) - 1
		}

		addFragToBudgetUnit(&units[home], i, taggedFrag, len(callIDs) > 0, len(resultIDs) > 0)

		for _, id := range callIDs {
			open[id] = home
		}
	}
	return units
}

// mergeBudgetUnits folds src into dest when a single fragment's result parts
// close calls that opened two different (still-open) units — a batched
// multi-result tool message answering more than one pending call. It also
// redirects any other ids still pointing at src, in case src had further
// calls open elsewhere.
func mergeBudgetUnits(units []budgetUnit, dest, src int, open map[string]int) {
	from := units[src]
	to := &units[dest]
	to.indexes = append(to.indexes, from.indexes...)
	to.droppable = to.droppable && from.droppable
	to.hasCall = to.hasCall || from.hasCall
	to.hasResult = to.hasResult || from.hasResult
	units[src] = budgetUnit{}
	for id, idx := range open {
		if idx == src {
			open[id] = dest
		}
	}
}

func addFragToBudgetUnit(unit *budgetUnit, index int, taggedFrag TaggedFrag, hasCall, hasResult bool) {
	unit.indexes = append(unit.indexes, index)
	if !taggedFrag.HasTag(TagCanDrop) {
		unit.droppable = false
	}
	if hasCall {
		unit.hasCall = true
	}
	if hasResult {
		unit.hasResult = true
	}
}

func fragToolCallIDs(frag contextfrag.ContextFrag) []string {
	var ids []string
	for _, part := range frag.Parts {
		msg := sdkMessagePart(part)
		if msg == nil {
			continue
		}
		for _, mp := range msg.Content {
			if call, ok := mp.(sdk.ToolCallPart); ok {
				if id := strings.TrimSpace(call.ToolCallID); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

func fragToolResultCallIDs(frag contextfrag.ContextFrag) []string {
	var ids []string
	for _, part := range frag.Parts {
		msg := sdkMessagePart(part)
		if msg == nil {
			continue
		}
		for _, mp := range msg.Content {
			if result, ok := mp.(sdk.ToolResultPart); ok {
				if id := strings.TrimSpace(result.ToolCallID); id != "" {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

// orphanToolExchangeDrops drops every droppable unit that carries only one
// side of a tool exchange; left in place it is a guaranteed provider 400.
func orphanToolExchangeDrops(tagged []TaggedFrag) (map[int]bool, map[int]string) {
	units := buildBudgetUnits(tagged)
	var drops map[int]bool
	var reasons map[int]string
	for i := range units {
		unit := &units[i]
		if !unit.droppable || !unit.orphanToolExchange() {
			continue
		}
		if drops == nil {
			drops = make(map[int]bool)
			reasons = make(map[int]string)
		}
		for _, idx := range unit.indexes {
			drops[idx] = true
			reasons[idx] = budgetDropReasonOrphanExchange
		}
	}
	return drops, reasons
}

// protectedHistoryTokenCost charges every protected non-system/non-current
// fragment to the unified history budget. Current requests may intentionally
// retain the history slot to preserve composed order, so kind is authoritative
// alongside slot. Unit protection is atomic: when a protected tool-call/result
// member makes the whole closure non-droppable, every history member of that
// closure is charged.
func protectedHistoryTokenCost(tagged []TaggedFrag) int {
	units := buildBudgetUnits(tagged)
	total := 0
	for i := range units {
		unit := &units[i]
		if unit.droppable {
			continue
		}
		for _, idx := range unit.indexes {
			frag := tagged[idx].Frag
			if frag.Slot == contextfrag.SlotSystem ||
				frag.Slot == contextfrag.SlotCurrentUser ||
				frag.Kind == contextfrag.KindCurrentUserMessage {
				continue
			}
			total += tagged[idx].Tokens
		}
	}
	return total
}
