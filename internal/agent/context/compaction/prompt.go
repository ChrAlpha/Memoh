package compaction

import (
	"fmt"
	"strings"
)

const systemPrompt = `You are a conversation summarizer. Given a conversation history, produce a concise summary that preserves:
- Key facts, decisions, and agreements
- User preferences and requests
- Important context needed for continuing the conversation
- Names, dates, numbers, and specific details
- Tool usage outcomes and their results

If <prior_context> is provided, it contains summaries of earlier conversation segments. Use them ONLY to understand the conversation flow and maintain continuity. Do NOT include, repeat, or rephrase any content from <prior_context> in your output.

For tool results, only include key outcomes; ignore intermediate steps or errors.

Output ONLY the summary of the new conversation segment. No preamble, no headers.`

const fusionSystemPrompt = `You are a conversation summarizer. Given absorbed earlier context and a new conversation segment, produce a concise summary that preserves:
- Key facts, decisions, and agreements
- User preferences and requests
- Important context needed for continuing the conversation
- Names, dates, numbers, and specific details
- Tool usage outcomes and their results

If <prior_context> is provided, it contains summaries that remain active after this pass. Use them ONLY to understand the conversation flow and maintain continuity. Do NOT include, repeat, or rephrase any content from <prior_context> in your output.

The output summary REPLACES every earlier summary in <absorbed_context> entirely. Integrate all still-relevant information from the absorbed context and the new conversation segment. Do not write references like "see prior summary" or otherwise depend on an earlier summary remaining available.

For tool results, only include key outcomes; ignore intermediate steps or errors.

Output ONLY the replacement summary. No preamble, no headers.`

type messageEntry struct {
	Role    string
	Content string
}

func buildUserPrompt(messages []messageEntry) string {
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation:\n")
	for _, m := range messages {
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, m.Content)
	}
	return sb.String()
}

func buildFusionUserPrompt(absorbed []absorbedSegment, messages []messageEntry) string {
	var sb strings.Builder
	sb.WriteString("<absorbed_context>\n")
	for i, segment := range absorbed {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(absorbedSegmentHeader(segment.Source))
		sb.WriteByte('\n')
		sb.WriteString(strings.TrimSpace(segment.Content))
		sb.WriteByte('\n')
	}
	sb.WriteString("</absorbed_context>\n\n")
	sb.WriteString("Now summarize the following conversation segment:\n")
	for _, message := range messages {
		fmt.Fprintf(&sb, "%s: %s\n", message.Role, message.Content)
	}
	return sb.String()
}

func absorbedSegmentHeader(source absorbedSegmentSource) string {
	if source == absorbedSourceRawTranscript {
		return "[raw transcript segment]"
	}
	return "[earlier summary segment]"
}

// priorSeparatorTokens covers the "\n---\n" joiner between absorbed segments.
const priorSeparatorTokens = 2

// capEntriesToBudget truncates entry contents so their total prompt cost fits
// maxTokens. Entries are never dropped — ids and entries are emitted together
// by buildEntriesAndIDs, so dropping one would mark a row the summarizer never
// saw. This only engages when a single markable group exceeds the whole
// budget: the alternative is a prompt the compaction model rejects on every
// pass, which stalls the session behind the failure cooldown forever.
//
// The bound is therefore best-effort, not absolute: every entry keeps at
// least its floor, so when one unsplittable group holds enough entries that
// the floors alone exceed maxTokens, the capped total still does. Callers
// that need the real cost recheck with entriesPromptCost.
func capEntriesToBudget(entries []messageEntry, maxTokens int) []messageEntry {
	if maxTokens <= 0 {
		maxTokens = 1
	}
	markerTokens := estimateBytesAsTokens(truncationMarker)
	overheadOf := func(entry messageEntry) int { return estimateBytesAsTokens(entry.Role) + 1 }
	costOf := func(entry messageEntry) int { return estimateBytesAsTokens(entry.Content) + overheadOf(entry) }
	// An entry's floor is its cheapest faithful representation: the original
	// text when it is already smaller than a truncation marker. Reserving
	// every later entry's floor up front means spending on one entry can never
	// push the tail past the cap, and short entries are never replaced by a
	// marker that costs more than they do.
	floorOf := func(entry messageEntry) int {
		if cost := costOf(entry); cost < overheadOf(entry)+markerTokens {
			return cost
		}
		return overheadOf(entry) + markerTokens
	}
	tailFloor := make([]int, len(entries)+1)
	for i := len(entries) - 1; i >= 0; i-- {
		tailFloor[i] = tailFloor[i+1] + floorOf(entries[i])
	}
	remaining := maxTokens
	capped := make([]messageEntry, len(entries))
	for i, entry := range entries {
		overhead := overheadOf(entry)
		avail := remaining - tailFloor[i+1]
		cost := costOf(entry)
		if cost > avail {
			truncated := entry
			budgetBytes := (avail - overhead) * 4
			if budgetBytes > len(truncationMarker) {
				truncated.Content = truncateBytes(entry.Content, budgetBytes-len(truncationMarker))
			} else {
				truncated.Content = truncationMarker
			}
			if truncatedCost := costOf(truncated); truncatedCost < cost {
				entry, cost = truncated, truncatedCost
			}
		}
		remaining -= cost
		capped[i] = entry
	}
	return capped
}

// entriesPromptCost is the estimated prompt cost of the rendered entries,
// using the same per-entry accounting as capEntriesToBudget.
func entriesPromptCost(entries []messageEntry) int {
	total := 0
	for _, entry := range entries {
		total += estimateBytesAsTokens(entry.Content) + estimateBytesAsTokens(entry.Role) + 1
	}
	return total
}
