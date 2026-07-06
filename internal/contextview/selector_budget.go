package contextview

import (
	"encoding/json"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

// HistoryTrimNotice tells the model that budget pressure removed messages.
// Tiered drops can leave holes mid-history, not only at the head, so the
// wording stays honest about that. The text is model-visible; change it only
// deliberately.
const HistoryTrimNotice = "[System Notice] Some earlier and intervening messages were trimmed to fit the context window. " +
	"If you need information from the trimmed messages, use the available tools " +
	"(such as memory_read or web search) to retrieve it."

// Budget drop reasons name the attention band a fragment fell out of, so the
// lifecycle drop_reasons histogram explains what budget pressure removed.
const (
	budgetDropReasonPassive      = "budget:passive"
	budgetDropReasonUntiered     = "budget:untiered"
	budgetDropReasonDirected     = "budget:directed"
	budgetDropReasonRecentWindow = "budget:recent_window"
	budgetDropReasonOrphanResult = "budget:orphan_tool_result"
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
// never break a tool closure. Units span the whole tagged set: when any
// member is protected the entire unit is (mixed droppability would orphan
// half a closure), and the tier comes from attention-bearing members only so
// data-less tool results never promote a passive exchange.
type budgetUnit struct {
	indexes       []int
	tokens        int
	attentionTier int
	hasAttention  bool
	droppable     bool
	hasCall       bool
	hasResult     bool
}

func (u *budgetUnit) tier() int {
	if u.hasAttention {
		return u.attentionTier
	}
	return attentionTierUntiered
}

func (u *budgetUnit) orphanResult() bool {
	return u.hasResult && !u.hasCall
}

func buildBudgetUnits(tagged []TaggedFrag) []budgetUnit {
	units := make([]budgetUnit, 0, len(tagged))
	unitByCall := make(map[string]int)
	for i, taggedFrag := range tagged {
		frag := taggedFrag.Frag
		unitIdx := -1
		for _, callID := range fragToolResultCallIDs(frag) {
			if existing, ok := unitByCall[callID]; ok {
				unitIdx = existing
				break
			}
		}
		if unitIdx < 0 {
			units = append(units, budgetUnit{droppable: true})
			unitIdx = len(units) - 1
		}
		unit := &units[unitIdx]
		unit.indexes = append(unit.indexes, i)
		unit.tokens += fragTokenEstimate(frag)
		if !taggedFrag.HasTag(TagCanDrop) {
			unit.droppable = false
		}
		if len(frag.Scope.Attention) > 0 {
			if tier := fragAttentionTier(frag); !unit.hasAttention || tier > unit.attentionTier {
				unit.attentionTier = tier
			}
			unit.hasAttention = true
		}
		if len(fragToolCallIDs(frag)) > 0 {
			unit.hasCall = true
		}
		if len(fragToolResultCallIDs(frag)) > 0 {
			unit.hasResult = true
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
// budget pressure. The drop order is a fixed total order that depends only on
// the fragments and the recent-protect window, never on the budget:
//
//  1. Droppable orphan tool-result units (no matching call anywhere in the
//     set) drop unconditionally: they are a guaranteed provider 400.
//  2. Units older than the recent-protect window drop tier by tier (passive,
//     then untiered, then directed), oldest first within a tier.
//  3. Units inside the window follow in the same tier order, reported as
//     budget:recent_window: the window yields last, and only under budgets
//     too small to hold it.
//
// Each spatial drop happens only while the droppable total still exceeds the
// budget — the fit check is the sole budget-dependent point. A larger budget
// stops earlier along the same sequence, so it always keeps a superset of
// what a smaller budget keeps, and dropping the whole sequence always reaches
// the budget because only droppable tokens are counted.
//
// Priority never enters retention: it only orders rendering. The budget
// counts droppable tokens only, mirroring the legacy trimMessagesByTokens
// accounting; when the droppable total fits, nothing drops and the output is
// byte-identical to the unbudgeted path.
func budgetTrimDrops(tagged []TaggedFrag, maxTokens, recentProtectTokens int) (map[int]bool, map[int]string) {
	if maxTokens <= 0 {
		return nil, nil
	}
	units := buildBudgetUnits(tagged)

	drops := make(map[int]bool)
	reasons := make(map[int]string)
	dropUnit := func(unit *budgetUnit, reason string) {
		for _, idx := range unit.indexes {
			drops[idx] = true
			reasons[idx] = reason
		}
	}

	pool := make([]int, 0, len(units))
	total := 0
	for i := range units {
		unit := &units[i]
		if !unit.droppable {
			continue
		}
		if unit.orphanResult() {
			dropUnit(unit, budgetDropReasonOrphanResult)
			continue
		}
		pool = append(pool, i)
		total += unit.tokens
	}
	if total <= maxTokens {
		if len(drops) == 0 {
			return nil, nil
		}
		return drops, reasons
	}

	protectedStart := len(pool)
	acc := 0
	for protectedStart > 0 && acc+units[pool[protectedStart-1]].tokens <= recentProtectTokens {
		acc += units[pool[protectedStart-1]].tokens
		protectedStart--
	}

	dropBand := func(band []int, reasonForTier func(int) string) bool {
		for tier := attentionTierPassive; tier <= attentionTierDirected; tier++ {
			for _, unitIdx := range band {
				if total <= maxTokens {
					return true
				}
				unit := &units[unitIdx]
				if unit.tier() != tier {
					continue
				}
				dropUnit(unit, reasonForTier(tier))
				total -= unit.tokens
			}
		}
		return total <= maxTokens
	}
	if !dropBand(pool[:protectedStart], budgetDropReasonForTier) {
		dropBand(pool[protectedStart:], func(int) string { return budgetDropReasonRecentWindow })
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
