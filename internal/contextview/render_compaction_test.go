package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
)

func TestCompactionRenderer_BasicEntries(t *testing.T) {
	t.Parallel()

	frags := []contextfrag.ContextFrag{
		compactionMessageFrag("user", sdk.UserMessage("hello")),
		compactionMessageFrag("assistant", sdk.AssistantMessage("hi")),
	}

	payload := renderCompaction(t, frags, nil)

	if payload.SystemPrompt == "" {
		t.Fatal("SystemPrompt should be set")
	}
	for _, want := range []string{"Summarize the following conversation:", "user: hello", "assistant: hi"} {
		if !strings.Contains(payload.UserPrompt, want) {
			t.Fatalf("UserPrompt missing %q:\n%s", want, payload.UserPrompt)
		}
	}
	assertRefIDs(t, payload.CandidateRefs, []string{"user", "assistant"})
}

func TestCompactionRenderer_SkipsEmptyContent(t *testing.T) {
	t.Parallel()

	reasoningOnly := compactionMessageFrag("reasoning", sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ReasoningPart{Text: "hidden chain"},
		},
	})
	visible := compactionMessageFrag("visible", sdk.AssistantMessage("visible"))

	payload := renderCompaction(t, []contextfrag.ContextFrag{reasoningOnly, visible}, nil)

	if strings.Contains(payload.UserPrompt, "hidden chain") || strings.Contains(payload.UserPrompt, "reasoning:") {
		t.Fatalf("reasoning-only content should not render:\n%s", payload.UserPrompt)
	}
	if !strings.Contains(payload.UserPrompt, "assistant: visible") {
		t.Fatalf("visible entry missing:\n%s", payload.UserPrompt)
	}
}

func TestCompactionRenderer_PreservesRefsForCoverage(t *testing.T) {
	t.Parallel()

	reasoningOnly := compactionMessageFrag("reasoning", sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{
			sdk.ReasoningPart{Text: "hidden chain"},
		},
	})
	visible := compactionMessageFrag("visible", sdk.AssistantMessage("visible"))

	payload := renderCompaction(t, []contextfrag.ContextFrag{reasoningOnly, visible}, nil)

	assertRefIDs(t, payload.CandidateRefs, []string{"reasoning", "visible"})
}

func TestCompactionRenderer_PriorSummaries(t *testing.T) {
	t.Parallel()

	payload := renderCompaction(t, []contextfrag.ContextFrag{
		compactionMessageFrag("new", sdk.UserMessage("new request")),
	}, []string{"old summary"})

	for _, want := range []string{"<prior_context>", "old summary", "Now summarize the following conversation segment:", "user: new request"} {
		if !strings.Contains(payload.UserPrompt, want) {
			t.Fatalf("UserPrompt missing %q:\n%s", want, payload.UserPrompt)
		}
	}
}

func TestCompactionRenderer_EmptyInput(t *testing.T) {
	t.Parallel()

	payload, hash := renderCompactionPayload(t, nil, nil)

	if payload.SystemPrompt == "" {
		t.Fatal("SystemPrompt should be set for empty input")
	}
	if !strings.Contains(payload.UserPrompt, "Summarize the following conversation:") {
		t.Fatalf("UserPrompt = %q, want empty conversation prompt", payload.UserPrompt)
	}
	if len(payload.CandidateRefs) != 0 {
		t.Fatalf("CandidateRefs = %#v, want none", payload.CandidateRefs)
	}
	if hash == "" {
		t.Fatal("ContentHash should be set")
	}
}

func renderCompaction(t *testing.T, frags []contextfrag.ContextFrag, summaries []string) *CompactionRenderedPayload {
	t.Helper()
	payload, _ := renderCompactionPayload(t, frags, summaries)
	return payload
}

func renderCompactionPayload(t *testing.T, frags []contextfrag.ContextFrag, summaries []string) (*CompactionRenderedPayload, string) {
	t.Helper()
	renderer := &CompactionPromptRenderer{PriorSummaries: summaries}
	rendered, err := renderer.Render(context.Background(), RenderInput{
		Intent:    contextfrag.IntentCompactionCandidates,
		Selected:  frags,
		Placement: placementFor(frags),
		Scope:     contextfrag.Scope{BotID: "bot-1"},
		Target:    contextfrag.RenderCompactionPrompt,
	})
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}
	payload, ok := rendered.Data.(*CompactionRenderedPayload)
	if !ok {
		t.Fatalf("Data type = %T, want *CompactionRenderedPayload", rendered.Data)
	}
	return payload, rendered.ContentHash
}

func assertRefIDs(t *testing.T, refs []contextfrag.ContextRef, want []string) {
	t.Helper()
	got := make([]string, len(refs))
	for i, ref := range refs {
		got[i] = ref.ID
	}
	if len(got) != len(want) {
		t.Fatalf("ref ids = %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ref ids = %#v, want %#v", got, want)
		}
	}
}

func compactionMessageFrag(id string, msg sdk.Message) contextfrag.ContextFrag {
	frag := messageFrag(id, msg)
	frag.Ref = contextfrag.ContextRef{
		Namespace:  "test",
		ID:         id,
		Schema:     contextfrag.SchemaContextRef,
		Durability: contextfrag.RefDurable,
	}
	return frag
}
