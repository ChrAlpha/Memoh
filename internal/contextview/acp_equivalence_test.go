package contextview

import (
	"context"
	"strings"
	"testing"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	pipeline "github.com/memohai/memoh/internal/chat/timeline"
)

// The discuss-ACP prompt renders from selected fragments. For streams where
// the legacy MergeContext would not fold adjacent RC segments, the fragment
// rendering must stay byte-identical to the legacy prompt.
func TestDiscussACPPromptMatchesLegacyForAlternatingStreams(t *testing.T) {
	t.Parallel()

	rc := pipeline.RenderedContext{
		{ReceivedAtMs: 100, Content: []pipeline.RenderedContentPiece{{Type: "text", Text: "hello"}}},
		{ReceivedAtMs: 300, Content: []pipeline.RenderedContentPiece{{Type: "text", Text: "how about now?"}}},
	}
	trs := []pipeline.TurnResponseEntry{
		{RequestedAtMs: 200, Role: "assistant", Content: "hi"},
	}

	for _, tc := range []struct {
		name        string
		summary     string
		lateBinding string
	}{
		{name: "with late binding", lateBinding: "Only answer if mentioned."},
		{name: "no late binding"},
		{name: "with summary", summary: "older context", lateBinding: "Mentioned in this turn."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			builder := &DiscussSDKContextBuilder{}
			got, err := builder.BuildDiscussACPPrompt(context.Background(), contextfrag.Scope{BotID: "bot-1"}, DiscussContextInput{
				RC:             rc,
				TRs:            trs,
				CompactSummary: tc.summary,
				LateBinding:    tc.lateBinding,
			})
			if err != nil {
				t.Fatalf("BuildDiscussACPPrompt failed: %v", err)
			}
			composed := pipeline.ComposeContext(rc, trs, tc.summary)
			if composed == nil {
				t.Fatal("composed should not be nil")
			}
			want := legacyDiscussACPFullContextPrompt(composed.Messages, tc.lateBinding)
			if got != want {
				t.Fatalf("prompt mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

// Adjacent RC segments are atomized into their own [user] blocks instead of
// being folded into one message. This is the deliberate contract change from
// the legacy MergeContext folding.
func TestDiscussACPPromptKeepsAdjacentSegmentsAtomized(t *testing.T) {
	t.Parallel()

	rc := pipeline.RenderedContext{
		{ReceivedAtMs: 100, Content: []pipeline.RenderedContentPiece{{Type: "text", Text: "first burst"}}},
		{ReceivedAtMs: 150, Content: []pipeline.RenderedContentPiece{{Type: "text", Text: "second burst"}}},
	}
	builder := &DiscussSDKContextBuilder{}
	got, err := builder.BuildDiscussACPPrompt(context.Background(), contextfrag.Scope{BotID: "bot-1"}, DiscussContextInput{RC: rc})
	if err != nil {
		t.Fatalf("BuildDiscussACPPrompt failed: %v", err)
	}
	if !strings.Contains(got, "[user]\nfirst burst") || !strings.Contains(got, "[user]\nsecond burst") {
		t.Fatalf("adjacent segments must render as separate blocks:\n%s", got)
	}
}

func TestDiscussACPPromptEmptyInput(t *testing.T) {
	t.Parallel()

	builder := &DiscussSDKContextBuilder{}
	got, err := builder.BuildDiscussACPPrompt(context.Background(), contextfrag.Scope{BotID: "bot-1"}, DiscussContextInput{})
	if err != nil {
		t.Fatalf("BuildDiscussACPPrompt failed: %v", err)
	}
	want := legacyDiscussACPFullContextPrompt(nil, "")
	if got != want {
		t.Fatalf("empty prompt mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
