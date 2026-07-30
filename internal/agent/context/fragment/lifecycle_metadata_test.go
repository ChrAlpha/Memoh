package contextfrag

import (
	"encoding/json"
	"testing"
)

func TestLifecycleSnapshotFromMetadata(t *testing.T) {
	t.Parallel()

	snapshot := LifecycleSnapshot{
		Version:   1,
		Counts:    ManifestCounts{Fragments: 3, TokenEstimate: 1200},
		Breakdown: []KindBreakdown{{Kind: KindConversationEvent, Fragments: 2, TokenEstimate: 900}},
		ToolDefs:  []ToolDefAccounting{{Provider: "mcp", Name: "jira_search", Bytes: 400, TokenEstimate: 100}},
	}
	raw, err := json.Marshal(map[string]any{MetadataContextLifecycleKey: snapshot})
	if err != nil {
		t.Fatal(err)
	}

	got, ok := LifecycleSnapshotFromMetadata(raw)
	if !ok {
		t.Fatal("expected snapshot to parse")
	}
	if got.Counts.TokenEstimate != 1200 || len(got.Breakdown) != 1 || len(got.ToolDefs) != 1 {
		t.Fatalf("parsed snapshot = %+v, want original composition fields", got)
	}
}

func TestLifecycleSnapshotFromMetadataAbsent(t *testing.T) {
	t.Parallel()

	if _, ok := LifecycleSnapshotFromMetadata(nil); ok {
		t.Fatal("nil metadata must not parse")
	}
	if _, ok := LifecycleSnapshotFromMetadata([]byte(`{"other":1}`)); ok {
		t.Fatal("metadata without the lifecycle key must not parse")
	}
	if _, ok := LifecycleSnapshotFromMetadata([]byte(`not-json`)); ok {
		t.Fatal("invalid JSON must not parse")
	}
}

func TestLifecycleHolderTracksPersistedAssistantAssociation(t *testing.T) {
	t.Parallel()

	holder := NewLifecycleHolder()
	holder.SetManifest(Manifest{View: ViewRunConfigPreProvider})
	holder.SetAssistantMessageID(" assistant-message-1 ")

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("expected lifecycle snapshot")
	}
	if snapshot.AssistantMessageID != "assistant-message-1" {
		t.Fatalf("assistant message id = %q, want assistant-message-1", snapshot.AssistantMessageID)
	}
}
