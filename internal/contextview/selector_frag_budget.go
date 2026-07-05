package contextview

import (
	"fmt"
	"unicode/utf8"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/textutil"
)

const fragBudgetTokenByteFactor = 4

type fragBudgetDrop struct {
	frag   contextfrag.ContextFrag
	reason string
}

func enforceFragBudgets(frags []contextfrag.ContextFrag) (kept []contextfrag.ContextFrag, dropped []fragBudgetDrop, edits []contextfrag.ContextEditTrace, warnings []contextfrag.ValidationWarning) {
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

func fragCharCount(frag contextfrag.ContextFrag) int {
	count := 0
	for _, part := range frag.Parts {
		switch part.Type {
		case contextfrag.PartText:
			count += utf8.RuneCountInString(part.Text)
		case contextfrag.PartSDKMessage:
			if msg := sdkMessagePart(part); msg != nil {
				for _, mp := range msg.Content {
					if tp, ok := mp.(sdk.TextPart); ok {
						count += utf8.RuneCountInString(tp.Text)
					}
				}
			}
		}
	}
	return count
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
	limit, byBytes := fragTrimLimit(frag.Budget)
	if limit <= 0 {
		return frag, false
	}
	parts := make([]contextfrag.Part, len(frag.Parts))
	copy(parts, frag.Parts)
	remaining := limit
	removedBytes := 0
	changed := false
	last := -1
	for i, part := range parts {
		original := part.Text
		var kept string
		if byBytes {
			kept = safeUTF8BytePrefix(original, remaining)
		} else {
			kept = textutil.TruncateRunes(original, remaining)
		}
		if kept != original {
			changed = true
			removedBytes += len(original) - len(kept)
			last = i
		}
		if byBytes {
			remaining -= len(kept)
		} else {
			remaining -= utf8.RuneCountInString(kept)
		}
		parts[i].Text = kept
	}
	if !changed {
		return frag, false
	}
	parts[last].Text += fmt.Sprintf("[trimmed: %d bytes]", removedBytes)
	frag.Parts = parts
	return frag, true
}

func fragTrimLimit(budget contextfrag.BudgetPolicy) (limit int, byBytes bool) {
	if budget.MaxChars > 0 {
		return budget.MaxChars, false
	}
	if budget.MaxTokens > 0 {
		return budget.MaxTokens * fragBudgetTokenByteFactor, true
	}
	return 0, false
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
