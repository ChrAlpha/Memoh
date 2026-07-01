package contextview

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
)

func TestACPEquivalence_DiscussFullContextPrompt(t *testing.T) {
	t.Parallel()

	messages := []pipeline.ContextMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	lateBinding := "Only answer if mentioned."

	assertACPDiscussPromptEquivalent(t, messages, lateBinding)
}

func TestACPEquivalence_DiscussFullContextPromptNoLateBinding(t *testing.T) {
	t.Parallel()

	messages := []pipeline.ContextMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}

	assertACPDiscussPromptEquivalent(t, messages, "")
}

func TestACPEquivalence_DiscussFullContextPromptWithSummary(t *testing.T) {
	t.Parallel()

	messages := []pipeline.ContextMessage{
		{Role: "user", Content: "[Conversation summary]\nolder context"},
		{Role: "user", Content: "latest question"},
		{Role: "assistant", Content: "latest answer"},
	}
	lateBinding := "Mentioned in this turn."

	assertACPDiscussPromptEquivalent(t, messages, lateBinding)
}

func TestACPEquivalence_EmptyDiscussMessages(t *testing.T) {
	t.Parallel()

	assertACPDiscussPromptEquivalent(t, nil, "")
}

func assertACPDiscussPromptEquivalent(t *testing.T, messages []pipeline.ContextMessage, lateBinding string) {
	t.Helper()
	payload, _ := renderACP(t, ACPRenderConfig{
		Mode:               ACPRenderModeDiscuss,
		DiscussMessages:    messages,
		DiscussLateBinding: lateBinding,
	}, contextfrag.IntentDiscussReply)

	want := legacyDiscussACPFullContextPrompt(messages, lateBinding)
	if payload.ContextMarkdown != want {
		t.Fatalf("ContextMarkdown mismatch:\ngot:\n%s\nwant:\n%s", payload.ContextMarkdown, want)
	}
}

func legacyDiscussACPFullContextPrompt(messages []pipeline.ContextMessage, lateBinding string) string {
	var b strings.Builder
	b.WriteString("You are replying in a discuss-mode conversation. The runtime is reset each turn, so use the complete context below as the source of truth.\n\n")
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "user"
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		b.WriteString("[")
		b.WriteString(role)
		b.WriteString("]\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	b.WriteString("Reply to the latest user-visible message when a response is appropriate.")
	if strings.TrimSpace(lateBinding) != "" {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(lateBinding))
	}
	return strings.TrimSpace(b.String())
}
