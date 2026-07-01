package contextview

import (
	"encoding/json"
	"reflect"
	"testing"
	_ "unsafe"

	"github.com/memohai/memoh/internal/compaction"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/historyfrag"
)

//go:linkname legacyRecordCandidatesFromRecords github.com/memohai/memoh/internal/compaction.recordCandidatesFromRecords
func legacyRecordCandidatesFromRecords([]historyfrag.HistoryRecord) []compaction.RecordCompactionCandidate

//go:linkname legacySplitRecordCandidatesByRatio github.com/memohai/memoh/internal/compaction.splitRecordCandidatesByRatio
func legacySplitRecordCandidatesByRatio([]compaction.RecordCompactionCandidate, int, int) []compaction.RecordCompactionCandidate

func TestCompactionSelectionEquivalenceWithLegacyCandidates(t *testing.T) {
	t.Parallel()

	records := []historyfrag.HistoryRecord{
		contextviewHistoryRecord("current-user", "user", "current instruction"),
		contextviewHistoryRecord("loop-1", "assistant", "loop step 1"),
		contextviewHistoryRecord("loop-2", "assistant", "loop step 2"),
		contextviewHistoryRecord("tail", "assistant", "latest tail"),
	}
	frags := make([]contextfrag.ContextFrag, 0, len(records))
	for _, record := range records {
		frags = append(frags, historyfrag.ToFrag(record))
	}

	legacyCandidates := legacyRecordCandidatesFromRecords(records)
	legacySelected := legacySplitRecordCandidatesByRatio(legacyCandidates, 400, 100)

	result := selectCompactionFrags(frags)
	rendered := renderCompaction(t, result.Selected, nil)

	if got, want := refIDs(rendered.CandidateRefs), legacyCandidateRefIDs(legacySelected); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate refs = %#v, want legacy %#v", got, want)
	}
}

func contextviewHistoryRecord(id, role, content string) historyfrag.HistoryRecord {
	tokens := 100
	return historyfrag.HistoryRecord{
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
			Content: mustContextviewJSON(content),
		},
		DBMessageID:       id,
		UsageOutputTokens: &tokens,
	}
}

func mustContextviewJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func legacyCandidateRefIDs(candidates []compaction.RecordCompactionCandidate) []string {
	out := make([]string, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.Record.Ref.ID
	}
	return out
}

func refIDs(refs []contextfrag.ContextRef) []string {
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = ref.ID
	}
	return out
}
