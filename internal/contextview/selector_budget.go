package contextview

import (
	"encoding/json"
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

// budgetTrimDrops replicates the legacy trimMessagesByTokens decision over
// droppable fragments: accumulate estimated tokens newest to oldest, cut off
// everything older once the budget is exceeded, then extend the cut past
// leading tool results so no orphaned tool message survives at the head.
func budgetTrimDrops(tagged []TaggedFrag, maxTokens int) map[int]bool {
	if maxTokens <= 0 {
		return nil
	}
	candidates := make([]int, 0, len(tagged))
	for i, taggedFrag := range tagged {
		if taggedFrag.HasTag(TagCanDrop) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	total := 0
	cutoff := 0
	for pos := len(candidates) - 1; pos >= 0; pos-- {
		total += fragTokenEstimate(tagged[candidates[pos]].Frag)
		if total > maxTokens {
			cutoff = pos + 1
			break
		}
	}
	for cutoff < len(candidates) && isToolResultFrag(tagged[candidates[cutoff]].Frag) {
		cutoff++
	}
	if cutoff == 0 {
		return nil
	}

	drops := make(map[int]bool, cutoff)
	for _, idx := range candidates[:cutoff] {
		drops[idx] = true
	}
	return drops
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
