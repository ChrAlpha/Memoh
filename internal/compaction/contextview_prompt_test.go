package compaction

import (
	"strings"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
)

func TestContextViewCompactionPromptGoldenSmall(t *testing.T) {
	t.Parallel()

	candidates := []RecordCompactionCandidate{
		{Record: testRecord("u1", "user", "hello", 10)},
		{Record: testRecord("a1", "assistant", "hi", 10)},
	}

	payload, err := contextViewCompactionPrompt(candidates, nil)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}

	const want = "Summarize the following conversation:\nuser: hello\nassistant: hi\n"
	if payload.UserPrompt != want {
		t.Fatalf("user prompt = %q, want golden %q", payload.UserPrompt, want)
	}
	if !strings.HasPrefix(payload.SystemPrompt, "You are a conversation summarizer.") {
		t.Fatalf("system prompt drifted: %q", payload.SystemPrompt[:60])
	}
	if payload.EntryCount != 2 {
		t.Fatalf("EntryCount = %d, want 2", payload.EntryCount)
	}
}

func TestContextViewCompactionPromptGoldenSpec(t *testing.T) {
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

	blobResult := testRecord("blob-1", "tool", "", 10)
	blobResult.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolCallId": "call-4",
		"toolName":   "dump",
		"output":     map[string]any{"type": "text", "value": strings.Repeat("x", 5000)},
	}})

	longText := strings.Repeat("lorem ipsum ", 500)
	longResult := testRecord("long-1", "tool", "", 10)
	longResult.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolCallId": "call-5",
		"toolName":   "readfile",
		"output":     map[string]any{"type": "text", "value": longText},
	}})

	mixedParts := testRecord("mixed-1", "assistant", "", 10)
	mixedParts.ModelMessage.Content = mustCompactionJSON([]map[string]any{
		{"type": "tool-call", "toolCallId": "call-6", "toolName": "search", "input": map[string]any{"q": "memoh"}},
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
		blobResult,
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

	payload, err := contextViewCompactionPrompt(candidates, priorSummaries)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}

	for _, want := range []string{
		"<prior_context>\n",
		"earlier summary one\n---\nearlier summary two",
		"</prior_context>\n\nNow summarize the following conversation segment:\n",
		"user: [message_id: ext-1]\n[reply_to: ext-0]\n[sender: Alice (ops)]\n[platform: telegram]\n[conversation_type: group]\n[conversation_name: Dev Chat]\n[reply_target: thread-9]\nhello with metadata\n",
		"tool: answer is 42\n",
		`tool: {"body":"payload","status":"ok"}` + "\n",
		"tool: [media] trailing\n",
		"tool: [media]\n",
		"tool: " + strings.TrimSpace(longText[:2048]) + " …[truncated]\n",
		"assistant: found it\n[tool_call: search]\n[image]\n[file]\n",
		"assistant: plain answer\n",
	} {
		if !strings.Contains(payload.UserPrompt, want) {
			t.Fatalf("prompt missing golden %q:\n%s", want, payload.UserPrompt)
		}
	}
	if strings.Contains(payload.UserPrompt, "hidden") || strings.Contains(payload.UserPrompt, "reason-1") {
		t.Fatal("reasoning-only entry should not render")
	}
	if payload.EntryCount != 8 {
		t.Fatalf("EntryCount = %d, want 8 (reasoning-only skipped)", payload.EntryCount)
	}
	if len(payload.CandidateRefs) != len(records) {
		t.Fatalf("refs = %d, want all %d for coverage", len(payload.CandidateRefs), len(records))
	}
	for i, ref := range payload.CandidateRefs {
		if ref.ID != records[i].Ref.ID {
			t.Fatalf("ref %d = %q, want %q", i, ref.ID, records[i].Ref.ID)
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

	payload, err := contextViewCompactionPrompt(candidates, nil)
	if err != nil {
		t.Fatalf("contextViewCompactionPrompt failed: %v", err)
	}
	const want = "assistant: calling tool\n[tool_call: ask_user]\n"
	if !strings.Contains(payload.UserPrompt, want) {
		t.Fatalf("prompt missing golden %q:\n%s", want, payload.UserPrompt)
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
