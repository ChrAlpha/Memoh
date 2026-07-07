package contextfrag

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMutationLedgerNilSafe(t *testing.T) {
	t.Parallel()

	var ledger *MutationLedger
	ledger.Record(MutationBackgroundSummary, "bytes=10")
	ledger.SetFinalInputHash("abc")
	if ledger.Records() != nil || ledger.FinalInputHash() != "" {
		t.Fatal("nil ledger must be inert")
	}
}

func TestMutationLedgerRecordsInOrder(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.Record(MutationBackgroundSummary, "bytes=10")
	ledger.Record(MutationMidTaskPrune, "pruned=3")
	ledger.SetFinalInputHash("hash-1")

	records := ledger.Records()
	if len(records) != 2 ||
		records[0].Kind != MutationBackgroundSummary ||
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
	for _, want := range []string{"cache_comparator_prefix_hash", "cache_read_tokens", "cache_write_tokens"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot schema must reserve %q for cache telemetry: %s", want, raw)
		}
	}
}

func TestLifecycleSnapshotJSONSplitsCacheComparatorAndDecoratedProviderHashes(t *testing.T) {
	holder := NewLifecycleHolder()
	holder.SetManifest(Manifest{
		View: ViewRunConfigPreProvider,
		CachePlan: &CachePlan{
			CacheComparatorPrefixHash:   "comparator-hash",
			DecoratedProviderPrefixHash: "decorated-hash",
		},
	})

	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("snapshot should be available after SetManifest")
	}
	if snapshot.CacheComparatorPrefixHash != "comparator-hash" {
		t.Fatalf("snapshot cache comparator prefix hash = %q, want comparator-hash", snapshot.CacheComparatorPrefixHash)
	}
	if snapshot.DecoratedProviderPrefixHash != "decorated-hash" {
		t.Fatalf("snapshot decorated provider prefix hash = %q, want decorated-hash", snapshot.DecoratedProviderPrefixHash)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, want := range []string{`"cache_comparator_prefix_hash":"comparator-hash"`, `"decorated_provider_prefix_hash":"decorated-hash"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot JSON missing %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "rendered_prefix_hash") {
		t.Fatalf("snapshot JSON must not contain the old rendered_prefix_hash key: %s", raw)
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

func TestMutationLedgerAppendStepSnapshotStampsCurrentAttempt(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0, PostPrepareInputHash: "hash-0"})
	ledger.AdvanceAttempt()
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0, PostPrepareInputHash: "hash-1"})

	steps := ledger.StepSnapshots()
	if len(steps) != 2 {
		t.Fatalf("steps = %#v, want 2", steps)
	}
	if steps[0].Attempt != 0 {
		t.Fatalf("steps[0].Attempt = %d, want 0", steps[0].Attempt)
	}
	if steps[1].Attempt != 1 {
		t.Fatalf("steps[1].Attempt = %d, want 1 after AdvanceAttempt", steps[1].Attempt)
	}
}

func TestMutationLedgerAdvanceAttemptStampsCacheUsageRecords(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.RecordCacheUsage(CacheUsageRecord{StepIndex: 0})
	ledger.AdvanceAttempt()
	ledger.RecordCacheUsage(CacheUsageRecord{StepIndex: 0})

	records := ledger.CacheUsageRecords()
	if len(records) != 2 {
		t.Fatalf("records = %#v, want 2", records)
	}
	if records[0].Attempt != 0 || records[1].Attempt != 1 {
		t.Fatalf("attempts = %d, %d, want 0, 1", records[0].Attempt, records[1].Attempt)
	}
}

func TestMutationLedgerModelInfo(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.SetModelInfo("claude-x", "anthropic-messages")
	model, clientType := ledger.ModelInfo()
	if model != "claude-x" || clientType != "anthropic-messages" {
		t.Fatalf("ModelInfo() = (%q, %q), want (claude-x, anthropic-messages)", model, clientType)
	}
}

func TestMutationLedgerLoopSelectionMode(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.SetLoopSelectionMode(LoopSelectionSuffixOnly)
	if got := ledger.LoopSelectionMode(); got != LoopSelectionSuffixOnly {
		t.Fatalf("LoopSelectionMode() = %q, want %q", got, LoopSelectionSuffixOnly)
	}
}

func TestMutationLedgerStepAttemptModelNilSafe(t *testing.T) {
	t.Parallel()

	var ledger *MutationLedger
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0})
	ledger.AdvanceAttempt()
	ledger.SetModelInfo("m", "c")
	ledger.SetLoopSelectionMode(LoopSelectionLegacyPrune)
	if ledger.StepSnapshots() != nil {
		t.Fatal("nil ledger StepSnapshots() must be nil")
	}
	if model, clientType := ledger.ModelInfo(); model != "" || clientType != "" {
		t.Fatalf("nil ledger ModelInfo() = (%q, %q), want empty", model, clientType)
	}
	if got := ledger.LoopSelectionMode(); got != "" {
		t.Fatalf("nil ledger LoopSelectionMode() = %q, want empty", got)
	}
}

func TestMutationLedgerMarshalJSONIncludesStepsModelAndLoopSelectionMode(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.Record(MutationMidTaskPrune, "truncated=2")
	ledger.SetFinalInputHash("final-hash")
	ledger.SetModelInfo("claude-x", "anthropic-messages")
	ledger.SetLoopSelectionMode(LoopSelectionSuffixOnly)
	ledger.AppendStepSnapshot(StepSnapshot{
		StepIndex:            0,
		PostPrepareInputHash: "step-hash-0",
		ReselectionApplied:   true,
		Dropped:              3,
		DropReasons:          map[string]int{"budget": 3},
	})
	ledger.AdvanceAttempt()
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 1, PostPrepareInputHash: "step-hash-1", Truncated: 2})

	raw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	for _, want := range []string{
		"mid_task_prune", "truncated=2", "final-hash",
		`"model":"claude-x"`, `"client_type":"anthropic-messages"`, `"loop_selection_mode":"suffix_only"`,
		`"step_index":0`, "step-hash-0", `"reselection_applied":true`, `"dropped":3`, `"budget":3`,
		`"step_index":1`, "step-hash-1", `"attempt":1`, `"truncated":2`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("ledger JSON missing %q:\n%s", want, raw)
		}
	}
}

func TestBuildLifecycleSnapshotCopiesModelClientTypeLoopSelectionModeAndSteps(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.SetModelInfo("claude-x", "anthropic-messages")
	ledger.SetLoopSelectionMode(LoopSelectionSuffixOnly)
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0, PostPrepareInputHash: "step-hash-0"})
	manifest := Manifest{Mutations: ledger}

	snapshot := BuildLifecycleSnapshot(manifest)
	if snapshot.Model != "claude-x" || snapshot.ClientType != "anthropic-messages" {
		t.Fatalf("snapshot model/client_type = (%q, %q), want (claude-x, anthropic-messages)", snapshot.Model, snapshot.ClientType)
	}
	if snapshot.LoopSelectionMode != LoopSelectionSuffixOnly {
		t.Fatalf("snapshot loop selection mode = %q, want %q", snapshot.LoopSelectionMode, LoopSelectionSuffixOnly)
	}
	if len(snapshot.Steps) != 1 || snapshot.Steps[0].PostPrepareInputHash != "step-hash-0" {
		t.Fatalf("snapshot steps = %#v", snapshot.Steps)
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, want := range []string{`"model":"claude-x"`, `"client_type":"anthropic-messages"`, `"loop_selection_mode":"suffix_only"`, `"steps":`, "step-hash-0"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("snapshot JSON missing %q: %s", want, raw)
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
