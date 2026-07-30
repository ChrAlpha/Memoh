package contextview

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/textutil"
)

const fragBudgetTokenByteFactor = 4

type fragBudgetDrop struct {
	frag   contextfrag.ContextFrag
	reason string
}

func enforceFragBudgets(
	frags []contextfrag.ContextFrag,
	profile IntentProfile,
	systemPlanActive bool,
) (kept []contextfrag.ContextFrag, dropped []fragBudgetDrop, edits []contextfrag.ContextEditTrace, warnings []contextfrag.ValidationWarning) {
	kept = make([]contextfrag.ContextFrag, 0, len(frags))
	for _, frag := range frags {
		reason, exceeded := fragBudgetExceeded(frag)
		if !exceeded {
			kept = append(kept, frag)
			continue
		}
		switch frag.Budget.Overflow {
		case contextfrag.OverflowDrop:
			if isToolExchangeFrag(frag) {
				kept = append(kept, frag)
				warnings = append(warnings, contextfrag.ValidationWarning{Code: "frag_budget_drop_blocked_tool_closure", Ref: frag.Ref})
				continue
			}
			if isMustKeepFrag(frag, profile) && (!systemPlanActive || !droppableSystemBudgetFrag(frag)) {
				kept = append(kept, frag)
				warnings = append(warnings, contextfrag.ValidationWarning{Code: "frag_budget_drop_blocked_must_keep", Ref: frag.Ref})
				continue
			}
			dropped = append(dropped, fragBudgetDrop{frag: frag, reason: reason})
		case contextfrag.OverflowTrim:
			if trimmed, ok := trimFragText(frag); ok {
				kept = append(kept, trimmed)
				edits = append(edits, fragBudgetTrimEdit(trimmed))
				continue
			}
			kept = append(kept, frag)
			warnings = append(warnings, contextfrag.ValidationWarning{Code: "overflow_trim_unsupported", Ref: frag.Ref})
		case contextfrag.OverflowSummarize:
			kept = append(kept, frag)
			warnings = append(warnings, contextfrag.ValidationWarning{Code: "overflow_summarize_unsupported", Ref: frag.Ref})
		default:
			kept = append(kept, frag)
		}
	}
	return kept, dropped, edits, warnings
}

func appendFragBudgetDrops(result SelectionResult, dropped []fragBudgetDrop, edits []contextfrag.ContextEditTrace, warnings []contextfrag.ValidationWarning) SelectionResult {
	result.Edited = append(result.Edited, edits...)
	result.Warnings = append(result.Warnings, warnings...)
	if len(dropped) == 0 {
		return result
	}
	for _, d := range dropped {
		result.Dropped = append(result.Dropped, d.frag)
		result.Summary.DropReasons = append(result.Summary.DropReasons, DropRecord{FragID: d.frag.ID, Ref: d.frag.Ref, Reason: d.reason})
	}
	result.Summary.TotalCollected += len(dropped)
	result.Summary.TotalDropped += len(dropped)
	return result
}

func fragBudgetExceeded(frag contextfrag.ContextFrag) (reason string, exceeded bool) {
	budget := frag.Budget
	if budget.MaxTokens <= 0 && budget.MaxChars <= 0 {
		return "", false
	}
	if budget.MaxTokens > 0 && fragTokenEstimate(frag) > budget.MaxTokens {
		return "frag_budget:max_tokens", true
	}
	if budget.MaxChars > 0 && fragCharCount(frag) > budget.MaxChars {
		return "frag_budget:max_chars", true
	}
	return "", false
}

// fragCharCount is the rune-count twin of the shared token estimator: text
// and reasoning count their runes, tool payloads count their serialized
// runes additively, images are excluded because their cost is not
// character-shaped.
func fragCharCount(frag contextfrag.ContextFrag) int {
	total := 0
	for _, part := range frag.Parts {
		switch part.Type {
		case contextfrag.PartText:
			total += utf8.RuneCountInString(part.Text)
		case contextfrag.PartSDKMessage:
			if msg := sdkMessagePart(part); msg != nil {
				for _, mp := range msg.Content {
					switch p := mp.(type) {
					case sdk.TextPart:
						total += utf8.RuneCountInString(p.Text)
					case sdk.ReasoningPart:
						total += utf8.RuneCountInString(p.Text)
					case sdk.ImagePart:
					default:
						if data, err := json.Marshal(mp); err == nil {
							total += utf8.RuneCountInString(string(data))
						}
					}
				}
			}
		}
	}
	return total
}

func isPureTextFrag(frag contextfrag.ContextFrag) bool {
	if len(frag.Parts) == 0 {
		return false
	}
	for _, part := range frag.Parts {
		if part.Type != contextfrag.PartText {
			return false
		}
	}
	return true
}

func trimFragText(frag contextfrag.ContextFrag) (contextfrag.ContextFrag, bool) {
	if !isPureTextFrag(frag) {
		return frag, false
	}
	charLimit, tokenByteLimit := fragTrimLimit(frag.Budget)
	if charLimit <= 0 && tokenByteLimit <= 0 {
		return frag, false
	}
	parts := frag.Parts
	changed := false
	if charLimit > 0 {
		if next, ok := trimPartsToLimit(parts, charLimit, false); ok {
			parts, changed = next, true
		}
	}
	if tokenByteLimit > 0 {
		if next, ok := trimPartsToLimit(parts, tokenByteLimit, true); ok {
			parts, changed = next, true
		}
	}
	if !changed {
		return frag, false
	}
	frag.Parts = parts
	frag.TokenEstimate = 0
	frag.Ref.ContentHash = ""
	frag.Ref.HashAlgo = ""
	frag.Ref.HashScope = ""
	return contextfrag.WithContextRef(frag, frag.Ref), true
}

func droppableSystemBudgetFrag(frag contextfrag.ContextFrag) bool {
	return frag.Slot == contextfrag.SlotSystem &&
		(frag.RetentionTier == contextfrag.RetentionOptional ||
			frag.RetentionTier == contextfrag.RetentionPreferred)
}

// fragTrimLimit returns the char-dimension (rune) and token-dimension (byte)
// trim limits configured on budget. A limit of 0 means that dimension is not
// configured, so trimFragText must skip its pass instead of treating the
// unconfigured dimension as satisfied.
func fragTrimLimit(budget contextfrag.BudgetPolicy) (charLimit int, tokenByteLimit int) {
	if budget.MaxChars > 0 {
		charLimit = budget.MaxChars
	}
	if budget.MaxTokens > 0 {
		tokenByteLimit = budget.MaxTokens * fragBudgetTokenByteFactor
	}
	return charLimit, tokenByteLimit
}

// trimPartsToLimit truncates parts' text to fit limit (runes when byBytes is
// false, bytes when true), reserving room for a "[trimmed from N bytes]"
// marker sized from parts' untrimmed length so the result never exceeds
// limit. When limit is too small to fit the marker itself, it degrades to a
// plain truncation with no marker.
func trimPartsToLimit(parts []contextfrag.Part, limit int, byBytes bool) ([]contextfrag.Part, bool) {
	originalBytes := 0
	for _, part := range parts {
		originalBytes += len(part.Text)
	}
	marker := fmt.Sprintf("[trimmed from %d bytes]", originalBytes)
	markerWidth := len(marker)
	contentLimit := limit
	appendMarker := limit > markerWidth
	if appendMarker {
		contentLimit = limit - markerWidth
	}
	next := make([]contextfrag.Part, len(parts))
	copy(next, parts)
	remaining := contentLimit
	changed := false
	last := -1
	for i, part := range next {
		original := part.Text
		var kept string
		if byBytes {
			kept = safeUTF8BytePrefix(original, remaining)
		} else {
			kept = textutil.TruncateRunes(original, remaining)
		}
		if kept != original {
			changed = true
			last = i
		}
		if byBytes {
			remaining -= len(kept)
		} else {
			remaining -= utf8.RuneCountInString(kept)
		}
		next[i].Text = kept
	}
	if !changed {
		return parts, false
	}
	if appendMarker {
		next[last].Text += marker
	}
	return next, true
}

func fragBudgetTrimEdit(frag contextfrag.ContextFrag) contextfrag.ContextEditTrace {
	ref := frag.Ref
	if err := contextfrag.ValidateContextRef(ref); err != nil {
		ref = contextfrag.WithContextRef(frag, ref).Ref
	}
	return contextfrag.ContextEditTrace{
		EditID: "frag_budget.trim." + frag.ID,
		Op:     contextfrag.EditReplace,
		Slot:   frag.Slot,
		Refs:   []contextfrag.ContextRef{ref},
	}
}

func safeUTF8BytePrefix(s string, maxBytes int) string {
	if maxBytes <= 0 || s == "" {
		return ""
	}
	if maxBytes >= len(s) {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
