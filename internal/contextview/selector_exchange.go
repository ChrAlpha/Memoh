package contextview

import (
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// orphanExchangeDropReason names a tool exchange left without its other half,
// in the lifecycle drop_reasons histogram.
const orphanExchangeDropReason = "budget:orphan_tool_exchange"

// exchangeUnit is a lone fragment, or an assistant tool-call fragment grouped
// with its tool-result fragments. When any member is protected the entire
// unit is: cutting half a closure would orphan the other half.
type exchangeUnit struct {
	indexes   []int
	droppable bool
	hasCall   bool
	hasResult bool
}

// orphanToolExchange reports a unit that carries only one side of a tool
// exchange: a call nothing ever answers, or a result answering a call that
// is not (or no longer) in the set. Either shape is a guaranteed provider
// 400 if it reaches the wire.
func (u *exchangeUnit) orphanToolExchange() bool {
	return u.hasCall != u.hasResult
}

// buildExchangeUnits pairs tool calls with their results in a single forward
// pass keyed by tool_call_id and scoped to appearance order, not id identity
// alone: a call opens its id, the next result seen for that id closes it,
// and the id is then free for a later call to reopen. Backends that emit
// deterministic per-turn ids (llama.cpp/vLLM style "call_0") reuse the same
// id across unrelated exchanges; scoping by the currently-open call keeps
// each exchange its own unit instead of collapsing every reuse of an id into
// one.
//
// A call whose id is already open when a new call for that id arrives does
// not close the earlier one: the earlier call stays open — and therefore
// orphaned, since nothing will ever close it — while the new call starts its
// own unit. A result whose id has no open call is likewise an orphan. This
// symmetric treatment also covers a result observed before its matching
// call: the result finds no open call (orphan), and the call that follows
// then opens a unit nothing ever closes (orphan too).
func buildExchangeUnits(tagged []TaggedFrag) []exchangeUnit {
	units := make([]exchangeUnit, 0, len(tagged))
	open := make(map[string]int)

	for i, taggedFrag := range tagged {
		callIDs := fragToolCallIDs(taggedFrag.Frag)
		resultIDs := fragToolResultCallIDs(taggedFrag.Frag)

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
				mergeExchangeUnits(units, home, idx, open)
			}
		}
		if home == -1 {
			units = append(units, exchangeUnit{droppable: true})
			home = len(units) - 1
		}

		unit := &units[home]
		unit.indexes = append(unit.indexes, i)
		unit.droppable = unit.droppable && taggedFrag.HasTag(TagCanDrop)
		unit.hasCall = unit.hasCall || len(callIDs) > 0
		unit.hasResult = unit.hasResult || len(resultIDs) > 0

		for _, id := range callIDs {
			open[id] = home
		}
	}
	return units
}

// mergeExchangeUnits folds src into dest when a single fragment's result
// parts close calls that opened two different (still-open) units — a batched
// multi-result tool message answering more than one pending call. It also
// redirects any other ids still pointing at src, in case src had further
// calls open elsewhere.
func mergeExchangeUnits(units []exchangeUnit, dest, src int, open map[string]int) {
	from := units[src]
	to := &units[dest]
	to.indexes = append(to.indexes, from.indexes...)
	to.droppable = to.droppable && from.droppable
	to.hasCall = to.hasCall || from.hasCall
	to.hasResult = to.hasResult || from.hasResult
	units[src] = exchangeUnit{}
	for id, idx := range open {
		if idx == src {
			open[id] = dest
		}
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

// orphanExchangeDrops cuts the droppable tool exchanges missing their other
// half, which no request may carry: left in place they are a guaranteed
// provider 400. It maps each dropped fragment's index to its reason.
func orphanExchangeDrops(tagged []TaggedFrag) map[int]string {
	var drops map[int]string
	for _, unit := range buildExchangeUnits(tagged) {
		if !unit.droppable || !unit.orphanToolExchange() {
			continue
		}
		if drops == nil {
			drops = make(map[int]string)
		}
		for _, idx := range unit.indexes {
			drops[idx] = orphanExchangeDropReason
		}
	}
	return drops
}
