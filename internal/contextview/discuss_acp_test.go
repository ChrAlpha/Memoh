package contextview

import (
	"context"
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
)

func discussACPInputFixture() pipeline.DiscussContextInput {
	return pipeline.DiscussContextInput{
		RC: pipeline.RenderedContext{
			{
				ReceivedAtMs: 100,
				Content:      []pipeline.RenderedContentPiece{{Type: "text", Text: "hello from alice"}},
			},
			{
				ReceivedAtMs: 300,
				Content:      []pipeline.RenderedContentPiece{{Type: "text", Text: "latest question"}},
			},
		},
		TRs: []pipeline.TurnResponseEntry{
			{RequestedAtMs: 200, Role: "assistant", Content: "earlier reply"},
		},
		LateBinding: "LATE_BINDING_INSTRUCTION",
	}
}

func TestBuildDiscussACPPromptRendersFromFragments(t *testing.T) {
	t.Parallel()

	builder := &DiscussSDKContextBuilder{}
	prompt, err := builder.BuildDiscussACPPrompt(
		context.Background(),
		contextfrag.Scope{BotID: "bot-1", SessionID: "session-1"},
		discussACPInputFixture(),
	)
	if err != nil {
		t.Fatalf("BuildDiscussACPPrompt error: %v", err)
	}

	for _, want := range []string{
		"discuss-mode conversation",
		"[user]\nhello from alice",
		"[assistant]\nearlier reply",
		"[user]\nlatest question",
		"Reply to the latest user-visible message",
		"LATE_BINDING_INSTRUCTION",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	aliceAt := strings.Index(prompt, "hello from alice")
	replyAt := strings.Index(prompt, "earlier reply")
	latestAt := strings.Index(prompt, "latest question")
	if aliceAt >= replyAt || replyAt >= latestAt {
		t.Fatalf("blocks out of time order:\n%s", prompt)
	}
	if lateAt := strings.Index(prompt, "LATE_BINDING_INSTRUCTION"); lateAt < strings.Index(prompt, "Reply to the latest") {
		t.Fatalf("late binding must come after the closing instruction:\n%s", prompt)
	}
	if strings.Contains(prompt, "[user]\nLATE_BINDING_INSTRUCTION") {
		t.Fatalf("late binding must not render as a conversation block:\n%s", prompt)
	}
}

func TestBuildDiscussACPPromptIncludesSummaryBlock(t *testing.T) {
	t.Parallel()

	input := discussACPInputFixture()
	input.CompactSummary = "OLDER_ROUNDS_SUMMARY"
	builder := &DiscussSDKContextBuilder{}
	prompt, err := builder.BuildDiscussACPPrompt(
		context.Background(),
		contextfrag.Scope{BotID: "bot-1"},
		input,
	)
	if err != nil {
		t.Fatalf("BuildDiscussACPPrompt error: %v", err)
	}
	if !strings.Contains(prompt, "OLDER_ROUNDS_SUMMARY") {
		t.Fatalf("prompt missing summary:\n%s", prompt)
	}
	if strings.Index(prompt, "OLDER_ROUNDS_SUMMARY") > strings.Index(prompt, "hello from alice") {
		t.Fatalf("summary must precede conversation blocks:\n%s", prompt)
	}
}
