package compaction

import (
	"encoding/json"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
)

func testRecord(id, role, content string, tokens int) historyfrag.HistoryRecord {
	record := historyfrag.HistoryRecord{
		Ref: contextfrag.ContextRef{
			Namespace:  historyfrag.NamespaceDBHistoryMessage,
			ID:         id,
			Schema:     contextfrag.SchemaContextRef,
			Durability: contextfrag.RefDurable,
		},
		Kind:       contextfrag.KindConversationEvent,
		SourceKind: historyfrag.SourceDBMessage,
		Lifecycle:  historyfrag.LifecyclePersisted,
		ModelMessage: conversation.ModelMessage{
			Role:    role,
			Content: conversation.NewTextContent(content),
		},
		DBMessageID: id,
	}
	if tokens > 0 {
		record.UsageOutputTokens = &tokens
	}
	return record
}

func toolCallRecord(id, toolName string, tokens int) historyfrag.HistoryRecord {
	record := testRecord(id, "assistant", "", tokens)
	record.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-call",
		"toolName":   toolName,
		"toolCallId": "call-1",
		"input":      map[string]any{},
	}})
	return record
}

func toolResultRecord(id, toolName string, tokens int) historyfrag.HistoryRecord {
	record := testRecord(id, "tool", "", tokens)
	record.ModelMessage.Content = mustCompactionJSON([]map[string]any{{
		"type":       "tool-result",
		"toolName":   toolName,
		"toolCallId": "call-1",
		"output":     map[string]any{"type": "text", "value": "42"},
	}})
	return record
}

func candidatesOf(records ...historyfrag.HistoryRecord) []RecordCompactionCandidate {
	out := make([]RecordCompactionCandidate, 0, len(records))
	for _, record := range records {
		out = append(out, RecordCompactionCandidate{Record: record})
	}
	return out
}

func candidateIDs(candidates []RecordCompactionCandidate) []string {
	out := make([]string, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.Record.Ref.ID
	}
	return out
}

func mustCompactionJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func sweepWindow() *contextview.CompactionWindow {
	return &contextview.CompactionWindow{SweepAll: true}
}

func TestSelectCompactionRecordsCompactsCurrentTurnMiddle(t *testing.T) {
	t.Parallel()

	candidates := candidatesOf(
		testRecord("current-user", "user", "current instruction", 100),
		testRecord("loop-1", "assistant", "loop step 1", 100),
		testRecord("loop-2", "assistant", "loop step 2", 100),
		testRecord("tail", "assistant", "latest tail", 100),
	)

	toCompact := selectCompactionRecords(candidates, sweepWindow())
	if len(toCompact) != 2 || toCompact[0].Record.Ref.ID != "loop-1" || toCompact[1].Record.Ref.ID != "loop-2" {
		t.Fatalf("should compact current-turn middle, got %#v", candidateIDs(toCompact))
	}
}

func TestSelectCompactionRecordsDoesNotOrphanToolResult(t *testing.T) {
	t.Parallel()

	candidates := candidatesOf(
		testRecord("context", "assistant", "context", 100),
		toolCallRecord("call", "calc", 100),
		toolResultRecord("result", "calc", 100),
		testRecord("tail", "assistant", "done", 100),
	)

	toCompact := selectCompactionRecords(candidates, &contextview.CompactionWindow{TargetTokens: 250})
	if len(toCompact) != 3 || toCompact[2].Record.Ref.ID != "result" {
		t.Fatalf("tool result should be pulled into compact side, got %#v", candidateIDs(toCompact))
	}
}

func TestSelectCompactionRecordsPreservesAskUser(t *testing.T) {
	t.Parallel()

	candidates := candidatesOf(
		testRecord("old-user", "user", "old question", 100),
		testRecord("old-assistant", "assistant", "old answer", 100),
		testRecord("current-user", "user", "current question", 100),
		toolCallRecord("ask-call", "ask_user", 100),
		toolResultRecord("ask-result", "ask_user", 100),
	)

	toCompact := selectCompactionRecords(candidates, sweepWindow())
	for _, candidate := range toCompact {
		if candidate.Record.Ref.ID == "ask-call" || candidate.Record.Ref.ID == "ask-result" {
			t.Fatalf("ask_user exchange must never be compacted, got %#v", candidateIDs(toCompact))
		}
	}
}

func TestSelectCompactionRecordsKeepRecentWindow(t *testing.T) {
	t.Parallel()

	candidates := candidatesOf(
		testRecord("old-1", "user", "old question", 100),
		testRecord("old-2", "assistant", "old answer", 100),
		testRecord("new-1", "user", "new question", 100),
		testRecord("new-2", "assistant", "new answer", 100),
	)

	// Keep the newest 150 tokens: new-2 (100) then new-1 reaches 200 >= 150,
	// so the cut lands after new-1 and only the two old records compact... the
	// guard then protects everything from the latest user turn onward.
	toCompact := selectCompactionRecords(candidates, &contextview.CompactionWindow{KeepRecentTokens: 150})
	if len(toCompact) != 2 || toCompact[0].Record.Ref.ID != "old-1" || toCompact[1].Record.Ref.ID != "old-2" {
		t.Fatalf("keep-recent window should compact only old records, got %#v", candidateIDs(toCompact))
	}
}

func TestSelectCompactionRecordsMaxPromptTokensTrimsOldest(t *testing.T) {
	t.Parallel()

	candidates := candidatesOf(
		testRecord("oldest", "user", "oldest content", 200),
		testRecord("middle", "assistant", "middle content", 200),
		testRecord("current-user", "user", "current question", 100),
		testRecord("tail", "assistant", "tail", 100),
	)

	window := sweepWindow()
	window.MaxPromptTokens = 250
	toCompact := selectCompactionRecords(candidates, window)
	if len(toCompact) != 1 || toCompact[0].Record.Ref.ID != "middle" {
		t.Fatalf("prompt budget should drop the oldest candidate, got %#v", candidateIDs(toCompact))
	}
}

func TestCompactionWindowFromConfig(t *testing.T) {
	t.Parallel()

	if _, ok := compactionWindowFromConfig(TriggerConfig{Ratio: 0, TotalInputTokens: 0}); ok {
		t.Fatal("no ratio and no target must disable compaction")
	}
	window, ok := compactionWindowFromConfig(TriggerConfig{TargetTokens: 500, MaxCompactTokens: 1000})
	if !ok || window.TargetTokens != 500 || window.MaxPromptTokens != 1000 {
		t.Fatalf("target window = %#v", window)
	}
	window, ok = compactionWindowFromConfig(TriggerConfig{Ratio: 100, TotalInputTokens: 4000})
	if !ok || !window.SweepAll || window.MaxPromptTokens != 30000 {
		t.Fatalf("sweep window = %#v", window)
	}
	window, ok = compactionWindowFromConfig(TriggerConfig{Ratio: 60, TotalInputTokens: 1000})
	if !ok || window.KeepRecentTokens != 400 {
		t.Fatalf("ratio window = %#v", window)
	}
}
