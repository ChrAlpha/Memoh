package compaction

import (
	"fmt"
	"strings"
)

const systemPrompt = `You compact an agent's conversation so its work can continue within a limited context window. Your summary replaces the content you are given: the next model call sees the summary in its place, followed by the messages kept verbatim.

Decide from the content itself what still matters and how long the summary needs to be. Condense repetition, routine output, and superseded detail heavily. Keep in full what later steps depend on:
- the user's goals, requests, preferences, and constraints that still apply
- decisions made and their reasons
- the state of the work: what is done, what is in progress, what comes next
- errors and failed attempts that are still relevant, so they are not repeated
- exact names, identifiers, paths, commands, URLs, numbers, and dates

Earlier summaries in <earlier_summary> are part of the content being replaced: carry forward whatever in them still matters, and do not refer to them.

The work continues from <current_request>, which stays verbatim after the summary: keep what it needs, and do not repeat it.

Output only the summary, with no preamble.`

type messageEntry struct {
	Role    string
	Content string
}

// summaryRoom is what the summarizer is told about the request its summary
// lands in.
type summaryRoom struct {
	HistoryTokens int
	KeptTokens    int
	SummaryTokens int
}

func buildUserPrompt(earlier []string, messages, current []messageEntry, room summaryRoom) string {
	var sb strings.Builder
	for _, summary := range earlier {
		sb.WriteString("<earlier_summary>\n")
		sb.WriteString(strings.TrimSpace(summary))
		sb.WriteString("\n</earlier_summary>\n\n")
	}
	writeEntries(&sb, "conversation", messages)
	writeEntries(&sb, "current_request", current)
	fmt.Fprintf(&sb, "The next request has room for about %d tokens of history. Messages kept verbatim after the summary take about %d of them, so the summary must stay under %d tokens; it can be much shorter when the content allows.\n",
		room.HistoryTokens, room.KeptTokens, room.SummaryTokens)
	return sb.String()
}

func writeEntries(sb *strings.Builder, tag string, entries []messageEntry) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(sb, "<%s>\n", tag)
	for _, m := range entries {
		fmt.Fprintf(sb, "%s: %s\n", m.Role, m.Content)
	}
	fmt.Fprintf(sb, "</%s>\n\n", tag)
}

// entriesPromptCost is the estimated prompt cost of the rendered entries.
func entriesPromptCost(entries []messageEntry) int {
	total := 0
	for _, entry := range entries {
		total += estimateBytesAsTokens(entry.Content) + estimateBytesAsTokens(entry.Role) + 1
	}
	return total
}

// earlierSummariesCost is the estimated prompt cost of the summaries a pass
// folds.
func earlierSummariesCost(earlier []string) int {
	total := 0
	for _, summary := range earlier {
		total += estimateBytesAsTokens("<earlier_summary>\n" + strings.TrimSpace(summary) + "\n</earlier_summary>\n\n")
	}
	return total
}
