package compaction

import (
	"encoding/json"
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
	"github.com/memohai/memoh/internal/userinput"
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

func TestRecordCandidatesAnnotateRecentToolAndAskUserPolicies(t *testing.T) {
	t.Parallel()

	candidates := recordCandidatesFromRecords([]historyfrag.HistoryRecord{
		testRecord("old-user", "user", "old question", 100),
		testRecord("old-assistant", "assistant", "old answer", 100),
		testRecord("current-user", "user", "current question", 100),
		toolCallRecord("ask-call", userinput.ToolNameAskUser, 100),
		toolResultRecord("ask-result", userinput.ToolNameAskUser, 100),
	})

	assertRecordPolicy(t, candidates[0], CompactPolicyCanDrop)
	assertRecordPolicy(t, candidates[1], CompactPolicyCanDrop)
	assertNoRecordPolicy(t, candidates[0], CompactPolicyPreserveRecent)
	assertRecordPolicy(t, candidates[2], CompactPolicyPreserveRecent)
	for _, idx := range []int{3, 4} {
		assertRecordPolicy(t, candidates[idx], CompactPolicyMustKeep)
		assertRecordPolicy(t, candidates[idx], CompactPolicyPreserveRecent)
		assertRecordPolicy(t, candidates[idx], CompactPolicyPreserveToolClosure)
		assertNoRecordPolicy(t, candidates[idx], CompactPolicyCanDrop)
	}
}

func TestSplitRecordCandidatesCompactsCurrentTurnMiddle(t *testing.T) {
	t.Parallel()

	candidates := recordCandidatesFromRecords([]historyfrag.HistoryRecord{
		testRecord("current-user", "user", "current instruction", 100),
		testRecord("loop-1", "assistant", "loop step 1", 100),
		testRecord("loop-2", "assistant", "loop step 2", 100),
		testRecord("tail", "assistant", "latest tail", 100),
	})

	toCompact := splitRecordCandidatesByRatio(candidates, 400, 100)
	if len(toCompact) != 2 {
		t.Fatalf("compact count = %d, want 2 middle current-turn records", len(toCompact))
	}
	if toCompact[0].Record.Ref.ID != "loop-1" || toCompact[1].Record.Ref.ID != "loop-2" {
		t.Fatalf("should compact current-turn middle, got %#v", candidateIDs(toCompact))
	}
}

func TestSplitRecordCandidatesDoesNotOrphanToolResult(t *testing.T) {
	t.Parallel()

	candidates := recordCandidatesFromRecords([]historyfrag.HistoryRecord{
		testRecord("context", "assistant", "context", 100),
		toolCallRecord("call", "calc", 100),
		toolResultRecord("result", "calc", 100),
		testRecord("tail", "assistant", "done", 100),
	})

	toCompact := splitRecordCandidatesByTarget(candidates, 250)
	if len(toCompact) != 3 {
		t.Fatalf("compact count = %d, want 3; got %#v", len(toCompact), candidateIDs(toCompact))
	}
	if toCompact[2].Record.Ref.ID != "result" {
		t.Fatalf("tool result should be pulled into compact side, got %#v", candidateIDs(toCompact))
	}
}

func TestBuildRecordEntriesAndRefsKeepsSelectedRefsForCoverage(t *testing.T) {
	t.Parallel()

	unrendered := testRecord("reasoning", "assistant", "", 0)
	unrendered.ModelMessage.Content = mustCompactionJSON([]map[string]any{{"type": "reasoning", "text": "hidden"}})
	rendered := testRecord("visible", "assistant", "visible", 0)
	candidates := recordCandidatesFromRecords([]historyfrag.HistoryRecord{unrendered, rendered})

	entries, refs := buildRecordEntriesAndRefs(candidates)
	if len(entries) != 1 || entries[0].Content != "visible" {
		t.Fatalf("entries = %#v, want only rendered row", entries)
	}
	if len(refs) != 2 || refs[0].ID != "reasoning" || refs[1].ID != "visible" {
		t.Fatalf("refs = %#v, want all selected refs for coverage/marking", refs)
	}
}

func assertRecordPolicy(t *testing.T, candidate RecordCompactionCandidate, policy CompactPolicy) {
	t.Helper()
	if !candidate.HasPolicy(policy) {
		t.Fatalf("candidate %s missing policy %s; got %#v", candidate.Record.Ref.ID, policy, candidate.Policies)
	}
}

func assertNoRecordPolicy(t *testing.T, candidate RecordCompactionCandidate, policy CompactPolicy) {
	t.Helper()
	if candidate.HasPolicy(policy) {
		t.Fatalf("candidate %s unexpectedly has policy %s; got %#v", candidate.Record.Ref.ID, policy, candidate.Policies)
	}
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
