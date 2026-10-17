package compaction

import (
	"strings"
	"testing"
)

func TestNonFusionPromptRemainsByteIdentical(t *testing.T) {
	t.Parallel()

	wantSystem := `You are a conversation summarizer. Given a conversation history, produce a concise summary that preserves:
- Key facts, decisions, and agreements
- User preferences and requests
- Important context needed for continuing the conversation
- Names, dates, numbers, and specific details
- Tool usage outcomes and their results

If <prior_context> is provided, it contains summaries of earlier conversation segments. Use them ONLY to understand the conversation flow and maintain continuity. Do NOT include, repeat, or rephrase any content from <prior_context> in your output.

For tool results, only include key outcomes; ignore intermediate steps or errors.

Output ONLY the summary of the new conversation segment. No preamble, no headers.`
	if systemPrompt != wantSystem {
		t.Fatalf("non-fusion system prompt changed:\n%s", systemPrompt)
	}

	wantUser := "Summarize the following conversation:\n" +
		"user: new question\nassistant: new answer\n"
	gotUser := buildUserPrompt(
		[]messageEntry{{Role: "user", Content: "new question"}, {Role: "assistant", Content: "new answer"}},
	)
	if gotUser != wantUser {
		t.Fatalf("non-fusion user prompt changed:\n%s", gotUser)
	}
}

func TestFusionPromptRendersAbsorbedContextWithoutPriorContext(t *testing.T) {
	t.Parallel()

	absorbed := []absorbedSegment{
		{Source: absorbedSourceRawTranscript, Content: "user: canonical raw"},
		{Source: absorbedSourceEarlierSummary, Content: "earlier condensed state"},
	}
	prompt := buildFusionUserPrompt(
		absorbed,
		[]messageEntry{{Role: "user", Content: "new conversation"}},
	)

	for _, want := range []string{
		"<absorbed_context>",
		"[raw transcript segment]",
		"[earlier summary segment]",
		"user: canonical raw",
		"earlier condensed state",
		"user: new conversation\n",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("fusion user prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "<prior_context>") {
		t.Fatalf("whole-frontier fusion prompt retained prior context:\n%s", prompt)
	}
	for _, want := range []string{
		"REPLACES",
		"Integrate all still-relevant information",
		"see prior summary",
	} {
		if !strings.Contains(fusionSystemPrompt, want) {
			t.Fatalf("fusion system prompt missing %q:\n%s", want, fusionSystemPrompt)
		}
	}
}

func TestCapEntriesToBudgetHoldsTheTotalAcrossManyEntries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		entries []messageEntry
		max     int
	}{
		{"two oversized", []messageEntry{
			{Role: "user", Content: strings.Repeat("a", 2400)},
			{Role: "assistant", Content: strings.Repeat("b", 2400)},
		}, 1000},
		{"one giant many small", []messageEntry{
			{Role: "user", Content: strings.Repeat("a", 4800)},
			{Role: "assistant", Content: strings.Repeat("b", 40)},
			{Role: "user", Content: strings.Repeat("c", 40)},
			{Role: "assistant", Content: strings.Repeat("d", 40)},
		}, 1000},
		{"tight budget", []messageEntry{
			{Role: "user", Content: strings.Repeat("a", 400)},
			{Role: "tool", Content: strings.Repeat("b", 400)},
			{Role: "assistant", Content: strings.Repeat("c", 400)},
		}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			capped := capEntriesToBudget(tc.entries, tc.max)
			if len(capped) != len(tc.entries) {
				t.Fatalf("entries dropped: got %d, want %d (ids must stay aligned)", len(capped), len(tc.entries))
			}
			total := 0
			for _, e := range capped {
				total += estimateBytesAsTokens(e.Content) + estimateBytesAsTokens(e.Role) + 1
			}
			if total > tc.max {
				t.Fatalf("capped total = %d tokens, exceeds max = %d", total, tc.max)
			}
		})
	}
}

// TestCapEntriesToBudgetFloorsCanExceedBudget pins the documented limit of
// the cap: entries are never dropped, so when the per-entry floors alone
// exceed maxTokens the capped total still does. entriesPromptCost is the
// recheck callers use to detect that overshoot.
func TestCapEntriesToBudgetFloorsCanExceedBudget(t *testing.T) {
	t.Parallel()

	entries := make([]messageEntry, 100)
	for i := range entries {
		entries[i] = messageEntry{Role: "tool", Content: "[tool result]"}
	}
	capped := capEntriesToBudget(entries, 50)
	if len(capped) != len(entries) {
		t.Fatalf("entries dropped: got %d, want %d (ids must stay aligned)", len(capped), len(entries))
	}
	if got := entriesPromptCost(capped); got <= 50 {
		t.Fatalf("floors of 100 minimal entries cannot fit 50 tokens, got %d — update the cap's documentation if this now holds", got)
	}
}

func TestCapEntriesToBudgetKeepsCheapShortEntries(t *testing.T) {
	t.Parallel()

	entries := []messageEntry{{Role: "user", Content: strings.Repeat("g", 4800)}}
	for i := 0; i < 20; i++ {
		entries = append(entries, messageEntry{Role: "tool", Content: "a"})
	}
	capped := capEntriesToBudget(entries, 100)
	total := 0
	for i, e := range capped {
		total += estimateBytesAsTokens(e.Content) + estimateBytesAsTokens(e.Role) + 1
		if i > 0 && e.Content != "a" {
			t.Fatalf("short entry %d was rewritten to %q: a marker must never replace cheaper original text", i, e.Content)
		}
	}
	if total > 100 {
		t.Fatalf("capped total = %d tokens, exceeds max = 100", total)
	}
}
