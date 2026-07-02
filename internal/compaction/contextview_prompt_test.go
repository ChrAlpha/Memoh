package compaction

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
)

func TestContextViewCompactionPromptMatchesLegacyByteForByte(t *testing.T) {
	t.Parallel()

	metadataRecord := testRecord("meta-1", "user", "hello with metadata", 10)
	metadataRecord.Scope = contextfrag.Scope{
		CurrentMessageID: "ext-1",
		ReplyToMessageID: "ext-0",
		DisplayName:      "Alice [ops]",
		Platform:         "telegram",
		ConversationType: "group",
		ConversationName: "Dev  Chat",
		ReplyTarget:      "thread-9",
	}
	metadataRecord.ExternalMessageID = "ext-1"
	metadataRecord.SourceReplyToMessageID = "ext-0"
	metadataRecord.SenderDisplayName = "Alice [ops]"
	metadataRecord.Platform = "telegram"

	outputStyleResult := testRecord("out-1", "tool", "", 10)
	outputStyleResult.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolCallId": "call-1",
		"toolName":   "lookup",
		"output":     map[string]any{"type": "text", "value": "answer is 42"},
	}})

	resultStyleResult := testRecord("res-1", "tool", "", 10)
	resultStyleResult.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolCallId": "call-2",
		"toolName":   "fetch",
		"result":     map[string]any{"status": "ok", "body": "payload"},
	}})

	mediaResult := testRecord("media-1", "tool", "", 10)
	mediaResult.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolCallId": "call-3",
		"toolName":   "screenshot",
		"output":     map[string]any{"type": "text", "value": "data:image/png;base64,aGVsbG8= trailing"},
	}})

	longResult := testRecord("long-1", "tool", "", 10)
	longResult.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolCallId": "call-4",
		"toolName":   "dump",
		"output":     map[string]any{"type": "text", "value": strings.Repeat("x", 5000)},
	}})

	mixedParts := testRecord("mixed-1", "assistant", "", 10)
	mixedParts.ModelMessage.Content = mustCompactionJSON([]map[string]any{
		{"type": "tool-call", "toolCallId": "call-5", "toolName": "search", "input": map[string]any{"q": "memoh"}},
		{"type": "text", "text": "found it"},
		{"type": "image"},
		{"type": "file"},
	})

	reasoningOnly := testRecord("reason-1", "assistant", "", 10)
	reasoningOnly.ModelMessage.Content = mustCompactionJSON([]map[string]any{{"type": "reasoning", "text": "hidden"}})

	records := []historyfrag.HistoryRecord{
		metadataRecord,
		outputStyleResult,
		resultStyleResult,
		mediaResult,
		longResult,
		mixedParts,
		reasoningOnly,
		testRecord("plain-1", "assistant", "plain answer", 10),
	}
	candidates := make([]RecordCompactionCandidate, 0, len(records))
	for _, record := range records {
		candidates = append(candidates, RecordCompactionCandidate{Record: record})
	}
	priorSummaries := []string{"earlier summary one", "earlier summary two"}

	legacyEntries, legacyRefs := buildRecordEntriesAndRefs(candidates)
	legacyPrompt := buildUserPrompt(priorSummaries, legacyEntries)

	payload, err := contextViewCompactionPrompt(candidates, priorSummaries)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}

	if payload.UserPrompt != legacyPrompt {
		t.Fatalf("user prompt diverged:\n--- contextview ---\n%s\n--- legacy ---\n%s", payload.UserPrompt, legacyPrompt)
	}
	if payload.SystemPrompt != systemPrompt {
		t.Fatal("system prompt diverged from legacy")
	}
	if payload.EntryCount != len(legacyEntries) {
		t.Fatalf("entry count = %d, want legacy %d", payload.EntryCount, len(legacyEntries))
	}
	if len(payload.CandidateRefs) != len(legacyRefs) {
		t.Fatalf("refs = %d, want legacy %d", len(payload.CandidateRefs), len(legacyRefs))
	}
	for i, ref := range payload.CandidateRefs {
		if ref.ID != legacyRefs[i].ID {
			t.Fatalf("ref %d = %q, want legacy %q", i, ref.ID, legacyRefs[i].ID)
		}
	}
}

func TestContextViewCompactionPromptEmptyEntries(t *testing.T) {
	t.Parallel()

	reasoningOnly := testRecord("reason-1", "assistant", "", 10)
	reasoningOnly.ModelMessage.Content = mustCompactionJSON([]map[string]any{{"type": "reasoning", "text": "hidden"}})
	candidates := []RecordCompactionCandidate{{Record: reasoningOnly}}

	payload, err := contextViewCompactionPrompt(candidates, nil)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}
	if payload.EntryCount != 0 {
		t.Fatalf("EntryCount = %d, want 0", payload.EntryCount)
	}
	if len(payload.CandidateRefs) != 1 {
		t.Fatalf("refs = %d, want ref preserved for coverage", len(payload.CandidateRefs))
	}
}

func TestContextViewSelectionDivergenceAcceptsLegacySplit(t *testing.T) {
	t.Parallel()

	candidates := recordCandidatesFromRecords([]historyfrag.HistoryRecord{
		testRecord("old-user", "user", "old question", 100),
		testRecord("old-assistant", "assistant", "old answer", 100),
		testRecord("current-user", "user", "current question", 100),
		testRecord("tail", "assistant", "tail answer", 100),
	})
	toCompact := splitRecordCandidatesByRatio(candidates, 400, 100)
	if len(toCompact) == 0 {
		t.Fatal("expected non-empty legacy split")
	}

	if divergent := contextViewSelectionDivergence(candidates, toCompact); len(divergent) != 0 {
		t.Fatalf("legacy split should be within contextview eligibility, divergent: %v", divergent)
	}
}

func TestContextViewCompactionPromptToolCallTopLevel(t *testing.T) {
	t.Parallel()

	topLevel := testRecord("top-1", "assistant", "calling tool", 10)
	topLevel.ModelMessage.ToolCalls = []conversation.ToolCall{{
		ID:   "call-9",
		Type: "function",
		Function: conversation.ToolCallFunction{
			Name:      "ask_user",
			Arguments: `{"question":"proceed?"}`,
		},
	}}
	candidates := []RecordCompactionCandidate{{Record: topLevel}}

	legacyEntries, _ := buildRecordEntriesAndRefs(candidates)
	legacyPrompt := buildUserPrompt(nil, legacyEntries)

	payload, err := contextViewCompactionPrompt(candidates, nil)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}
	if payload.UserPrompt != legacyPrompt {
		t.Fatalf("top-level tool call prompt diverged:\n--- contextview ---\n%s\n--- legacy ---\n%s", payload.UserPrompt, legacyPrompt)
	}
	if !strings.Contains(payload.UserPrompt, "[tool_call: ask_user]") {
		t.Fatalf("prompt missing tool call marker:\n%s", payload.UserPrompt)
	}
}

func TestContextViewSelectionEquivalenceWithLegacyCandidates(t *testing.T) {
	t.Parallel()

	records := []historyfrag.HistoryRecord{
		testRecord("current-user", "user", "current instruction", 100),
		testRecord("loop-1", "assistant", "loop step 1", 100),
		testRecord("loop-2", "assistant", "loop step 2", 100),
		testRecord("tail", "assistant", "latest tail", 100),
	}
	legacyCandidates := recordCandidatesFromRecords(records)
	legacySelected := splitRecordCandidatesByRatio(legacyCandidates, 400, 100)

	frags := make([]contextfrag.ContextFrag, 0, len(records))
	for _, record := range records {
		frags = append(frags, historyfrag.ToFrag(record))
	}
	selector := &contextview.FragmentSelector{}
	profile := selector.ProfileFor(contextfrag.IntentCompactionCandidates)
	result := selector.Select(frags, profile, contextview.BudgetEnvelope{})

	if len(result.Selected) != len(legacySelected) {
		t.Fatalf("selected = %d, want legacy %d", len(result.Selected), len(legacySelected))
	}
	for i, frag := range result.Selected {
		if frag.Ref.ID != legacySelected[i].Record.Ref.ID {
			t.Fatalf("selected %d = %q, want legacy %q", i, frag.Ref.ID, legacySelected[i].Record.Ref.ID)
		}
	}
}
