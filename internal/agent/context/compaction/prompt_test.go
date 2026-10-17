package compaction

import (
	"strings"
	"testing"
)

func TestPromptStatesTheRoomAndLeavesLengthToTheModel(t *testing.T) {
	t.Parallel()

	prompt := buildUserPrompt(
		[]string{"earlier state"},
		[]messageEntry{{Role: "user", Content: "new question"}, {Role: "tool", Content: "exit 1: permission denied"}},
		summaryRoom{HistoryTokens: 9000, KeptTokens: 1200, SummaryTokens: 7000},
	)
	for _, want := range []string{
		"<earlier_summary>\nearlier state\n</earlier_summary>",
		"<conversation>\nuser: new question\ntool: exit 1: permission denied\n</conversation>",
		"room for about 9000 tokens of history",
		"take about 1200 of them",
		"must stay under 7000 tokens",
		"much shorter when the content allows",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("user prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(systemPrompt, "ignore intermediate steps or errors") || !strings.Contains(systemPrompt, "failed attempts") {
		t.Fatalf("system prompt must keep failures that later steps depend on:\n%s", systemPrompt)
	}
}
