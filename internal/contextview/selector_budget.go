package contextview

import (
	"encoding/json"
	"sort"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

// HistoryTrimNotice mirrors the legacy trim notice injected when history is
// dropped to fit the context budget. The wording must stay byte-identical to
// the legacy resolver notice.
const HistoryTrimNotice = "[System Notice] Earlier conversation history has been trimmed to fit the context window. " +
	"If you need information from earlier in the conversation, use the available tools " +
	"(such as memory_read or web search) to retrieve it."

// Budget drop reasons name the attention band a fragment fell out of, so the
// lifecycle drop_reasons histogram explains what budget pressure removed.
const (
	budgetDropReasonPassive     = "budget:passive"
	budgetDropReasonUntiered    = "budget:untiered"
	budgetDropReasonDirected    = "budget:directed"
	budgetDropReasonWindowYield = "budget:recent_window_yield"
)

// Attention tiers order budget drops: passive group traffic goes first,
// fragments without attention data stay time-neutral in the middle, and
// directed traffic (mention/reply/direct/command/schedule/heartbeat) goes
// last.
const (
	attentionTierPassive = iota
	attentionTierUntiered
	attentionTierDirected
)

func fragAttentionTier(frag contextfrag.ContextFrag) int {
	reasons := frag.Scope.Attention
	if len(reasons) == 0 {
		return attentionTierUntiered
	}
	for _, reason := range reasons {
		if reason != contextfrag.AttentionPassive {
			return attentionTierDirected
		}
	}
	return attentionTierPassive
}

func budgetDropReasonForTier(tier int) string {
	switch tier {
	case attentionTierPassive:
		return budgetDropReasonPassive
	case attentionTierDirected:
		return budgetDropReasonDirected
	default:
		return budgetDropReasonUntiered
	}
}

// budgetUnit is the atomic drop unit: a lone fragment, or an assistant
// tool-call fragment grouped with its tool-result fragments so budget drops
// never break a tool closure.
type budgetUnit struct {
	indexes  []int
	tokens   int
	tier     int
	priority int
}

func buildBudgetUnits(tagged []TaggedFrag, candidates []int) []budgetUnit {
	units := make([]budgetUnit, 0, len(candidates))
	unitByCall := make(map[string]int)
	for _, idx := range candidates {
		frag := tagged[idx].Frag
		unitIdx := -1
		for _, callID := range fragToolResultCallIDs(frag) {
			if existing, ok := unitByCall[callID]; ok {
				unitIdx = existing
				break
			}
		}
		if unitIdx < 0 {
			units = append(units, budgetUnit{tier: fragAttentionTier(frag), priority: frag.Priority})
			unitIdx = len(units) - 1
		}
		unit := &units[unitIdx]
		unit.indexes = append(unit.indexes, idx)
		unit.tokens += fragTokenEstimate(frag)
		if tier := fragAttentionTier(frag); tier > unit.tier {
			unit.tier = tier
		}
		if frag.Priority > unit.priority {
			unit.priority = frag.Priority
		}
		for _, callID := range fragToolCallIDs(frag) {
			unitByCall[callID] = unitIdx
		}
	}
	return units
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

// budgetTrimDrops decides which droppable fragments leave the context under
// budget pressure using a three-band model over tool-closure units:
//
//  1. The newest units within recentProtectTokens are kept unconditionally;
//     when that window alone exceeds the budget it yields from its old end.
//  2. Units outside the window drop in composite order: attention tier first
//     (passive, then untiered, then directed), lower Priority first within a
//     tier, oldest first on full ties.
//  3. Without attention data or a window every unit is untiered, so the drop
//     set degenerates to the legacy oldest-first cut.
//
// The budget counts droppable tokens only, mirroring the legacy
// trimMessagesByTokens accounting; when the droppable total fits, nothing
// drops and the output is byte-identical to the unbudgeted path.
func budgetTrimDrops(tagged []TaggedFrag, maxTokens, recentProtectTokens int) (map[int]bool, map[int]string) {
	if maxTokens <= 0 {
		return nil, nil
	}
	candidates := make([]int, 0, len(tagged))
	for i, taggedFrag := range tagged {
		if taggedFrag.HasTag(TagCanDrop) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	units := buildBudgetUnits(tagged, candidates)
	total := 0
	for _, unit := range units {
		total += unit.tokens
	}
	if total <= maxTokens {
		return nil, nil
	}

	windowStart := len(units)
	if recentProtectTokens > 0 {
		windowStart = 0
		acc := 0
		for i := len(units) - 1; i >= 0; i-- {
			acc += units[i].tokens
			if acc > recentProtectTokens {
				windowStart = i + 1
				break
			}
		}
	}
	poolEnd := windowStart

	drops := make(map[int]bool)
	reasons := make(map[int]string)
	dropUnit := func(unit budgetUnit, reason string) {
		for _, idx := range unit.indexes {
			drops[idx] = true
			reasons[idx] = reason
		}
	}

	windowTokens := 0
	for _, unit := range units[windowStart:] {
		windowTokens += unit.tokens
	}
	for windowStart < len(units) && windowTokens > maxTokens {
		dropUnit(units[windowStart], budgetDropReasonWindowYield)
		windowTokens -= units[windowStart].tokens
		windowStart++
	}

	remaining := maxTokens - windowTokens
	poolTokens := 0
	for _, unit := range units[:poolEnd] {
		poolTokens += unit.tokens
	}
	if poolTokens > remaining {
		order := make([]int, poolEnd)
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			ua, ub := units[order[a]], units[order[b]]
			if ua.tier != ub.tier {
				return ua.tier < ub.tier
			}
			if ua.priority != ub.priority {
				return ua.priority < ub.priority
			}
			return ua.indexes[0] < ub.indexes[0]
		})
		for _, unitIdx := range order {
			if poolTokens <= remaining {
				break
			}
			dropUnit(units[unitIdx], budgetDropReasonForTier(units[unitIdx].tier))
			poolTokens -= units[unitIdx].tokens
		}
	}
	return drops, reasons
}

func fragTokenEstimate(frag contextfrag.ContextFrag) int {
	if frag.TokenEstimate > 0 {
		return frag.TokenEstimate
	}
	texts := make([]string, 0, len(frag.Parts))
	var fallback int
	for _, part := range frag.Parts {
		switch part.Type {
		case contextfrag.PartText:
			if strings.TrimSpace(part.Text) != "" {
				texts = append(texts, part.Text)
			}
		case contextfrag.PartSDKMessage:
			if msg := sdkMessagePart(part); msg != nil {
				for _, mp := range msg.Content {
					switch p := mp.(type) {
					case sdk.TextPart:
						if strings.TrimSpace(p.Text) != "" {
							texts = append(texts, p.Text)
						}
					default:
						if data, err := json.Marshal(mp); err == nil {
							fallback += len(data)
						}
					}
				}
			}
		}
	}
	if len(texts) > 0 {
		return len(strings.Join(texts, "\n")) / 4
	}
	return fallback / 4
}

// TrimNoticeFrag is the synthetic fragment the builder splices in when budget
// trimming dropped history, mirroring the legacy resolver notice message.
func TrimNoticeFrag(scope contextfrag.Scope) contextfrag.ContextFrag {
	msg := sdk.Message{
		Role:    sdk.MessageRoleSystem,
		Content: []sdk.MessagePart{sdk.TextPart{Text: HistoryTrimNotice}},
	}
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "history.trim_notice",
		Message:    msg,
		Kind:       contextfrag.KindSystemPolicy,
		Slot:       contextfrag.SlotHistory,
		Priority:   30,
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustSystem,
		Scope:      scope,
		Source:     "context_select",
		SourceID:   "history.trim_notice",
		Collector:  "budget_trim",
	})
}
