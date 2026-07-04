package contextfrag

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMutationLedgerNilSafe(t *testing.T) {
	t.Parallel()

	var ledger *MutationLedger
	ledger.Record(MutationToolUsageAppend, "bytes=10")
	ledger.SetFinalInputHash("abc")
	if ledger.Records() != nil || ledger.FinalInputHash() != "" {
		t.Fatal("nil ledger must be inert")
	}
}

func TestMutationLedgerRecordsInOrder(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.Record(MutationToolUsageAppend, "bytes=10")
	ledger.Record(MutationMidTaskPrune, "pruned=3")
	ledger.SetFinalInputHash("hash-1")

	records := ledger.Records()
	if len(records) != 2 ||
		records[0].Kind != MutationToolUsageAppend ||
		records[1].Kind != MutationMidTaskPrune {
		t.Fatalf("records = %#v", records)
	}
	if ledger.FinalInputHash() != "hash-1" {
		t.Fatalf("final hash = %q", ledger.FinalInputHash())
	}
}

func TestProviderInputHashDeterministic(t *testing.T) {
	t.Parallel()

	first := ProviderInputHash("system", []string{"a", "b"})
	second := ProviderInputHash("system", []string{"a", "b"})
	changed := ProviderInputHash("system", []string{"a", "c"})
	if first == "" || first != second {
		t.Fatal("hash must be deterministic and non-empty")
	}
	if first == changed {
		t.Fatal("hash must track payload changes")
	}
}

func TestManifestJSONIncludesLifecycle(t *testing.T) {
	ledger := NewMutationLedger()
	ledger.Record(MutationMidTaskPrune, "pruned=2")
	ledger.SetFinalInputHash("final-hash")
	manifest := Manifest{
		View:      ViewRunConfigPreProvider,
		CachePlan: &CachePlan{StablePrefixHash: "prefix-hash", StableMessageCount: 3},
		Mutations: ledger,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	for _, want := range []string{"prefix-hash", "mid_task_prune", "pruned=2", "final-hash"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("manifest JSON missing %q:\n%s", want, raw)
		}
	}
}

func TestLifecycleHolderSnapshotCondensesManifest(t *testing.T) {
	ledger := NewMutationLedger()
	ledger.Record(MutationMidTaskPrune, "pruned=2")
	ledger.SetFinalInputHash("final-hash")
	holder := NewLifecycleHolder()
	holder.SetManifest(Manifest{
		View: ViewRunConfigPreProvider,
		Counts: ManifestCounts{
			Fragments: 4,
			Messages:  2,
			Images:    1,
			TextBytes: 512,
		},
		Selection: &SelectionTrace{
			Selected:    3,
			Dropped:     1,
			DropReasons: map[string]int{"budget_trim": 1},
		},
		CachePlan: &CachePlan{StablePrefixHash: "prefix-hash", StableMessageCount: 2},
		Mutations: ledger,
		Items: []ManifestItem{
			{ID: "large-item", Kind: KindConversationEvent},
		},
	})

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("snapshot should be available after SetManifest")
	}
	if snapshot.View != ViewRunConfigPreProvider {
		t.Fatalf("snapshot view = %q", snapshot.View)
	}
	if snapshot.Counts.Messages != 2 || snapshot.Selection.Dropped != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.StablePrefixHash != "prefix-hash" || snapshot.FinalInputHash != "final-hash" {
		t.Fatalf("snapshot lost cache/final hash fields: %#v", snapshot)
	}
	if len(snapshot.Mutations) != 1 || snapshot.Mutations[0].Kind != MutationMidTaskPrune {
		t.Fatalf("snapshot mutations = %#v", snapshot.Mutations)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), "large-item") {
		t.Fatalf("condensed snapshot should not include manifest items: %s", raw)
	}
	for _, want := range []string{"rendered_prefix_hash", "cache_read_tokens", "cache_write_tokens"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot schema must reserve %q for cache telemetry: %s", want, raw)
		}
	}
}

func TestLifecycleSnapshotIncludesCacheUsage(t *testing.T) {
	ledger := NewMutationLedger()
	ledger.RecordCacheUsage(CacheUsageRecord{
		StepIndex:        0,
		CacheReadTokens:  11,
		CacheWriteTokens: 7,
	})
	holder := NewLifecycleHolder()
	holder.SetManifest(Manifest{
		View:      ViewRunConfigPreProvider,
		Mutations: ledger,
	})

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("snapshot should be available")
	}
	if snapshot.CacheReadTokens != 11 || snapshot.CacheWriteTokens != 7 {
		t.Fatalf("cache usage = read %d write %d", snapshot.CacheReadTokens, snapshot.CacheWriteTokens)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, want := range []string{`"step_index":0`, `"cache_read_tokens":11`, `"cache_write_tokens":7`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("cache usage JSON missing %s: %s", want, raw)
		}
	}
}

func TestManifestJSONIncludesCacheComparison(t *testing.T) {
	ledger := NewMutationLedger()
	ledger.SetCacheComparison(CacheComparison{Outcome: CacheOutcomeMissSamePrefix, PrevAgeMs: 1200})
	manifest := Manifest{Mutations: ledger}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	for _, want := range []string{"miss_same_prefix", "prev_age_ms"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("manifest JSON missing %q:\n%s", want, raw)
		}
	}
	snapshot := BuildLifecycleSnapshot(manifest)
	if snapshot.CacheComparison == nil || snapshot.CacheComparison.Outcome != CacheOutcomeMissSamePrefix {
		t.Fatalf("snapshot comparison = %#v", snapshot.CacheComparison)
	}
}
