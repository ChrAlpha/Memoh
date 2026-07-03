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
