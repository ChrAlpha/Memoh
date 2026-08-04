package contextfrag

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLifecycleHolderKeepsContentLightBudgetAudit(t *testing.T) {
	t.Parallel()

	holder := NewLifecycleHolder()
	if _, ok := holder.Snapshot(); ok {
		t.Fatal("empty holder unexpectedly exposed a snapshot")
	}
	ledger := NewMutationLedger()
	ledger.Record(MutationKind("context_budget_failure"), "protected_context_overflow")
	plan := ContextBudgetPlan{Window: 1024, SystemBudget: 256, ActualSystemCost: 930}
	holder.SetManifest(Manifest{
		View:               ViewRunConfigPreProvider,
		Counts:             ManifestCounts{Fragments: 2, Messages: 1, TextBytes: 2048},
		Items:              []ManifestItem{{ID: "private-content-marker"}},
		Selection:          &SelectionTrace{Selected: 1, Dropped: 1, DropReasons: map[string]int{"system_budget": 1}},
		SelectionDecisions: []SelectionDecision{{ID: "system.optional", Decision: DecisionDropped, Reason: "system_budget"}},
		BudgetPlan:         &plan,
		Mutations:          ledger,
	})

	snapshot, ok := holder.Snapshot()
	if !ok || snapshot.BudgetPlan == nil || snapshot.BudgetPlan.ActualSystemCost != 930 {
		t.Fatalf("snapshot = %#v, ok = %v", snapshot, ok)
	}
	if snapshot.Selection.DropReasons["system_budget"] != 1 || len(snapshot.SelectionDecisions) != 1 {
		t.Fatalf("selection audit = %#v / %#v", snapshot.Selection, snapshot.SelectionDecisions)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-content-marker") || strings.Contains(string(raw), `"items"`) {
		t.Fatalf("content-light snapshot leaked manifest items: %s", raw)
	}

	ledger.SetFinalInputHash("final-hash")
	refreshed, _ := holder.Snapshot()
	if refreshed.FinalInputHash != "final-hash" {
		t.Fatalf("live final hash = %q", refreshed.FinalInputHash)
	}
	refreshed.Selection.DropReasons["system_budget"] = 99
	again, _ := holder.Snapshot()
	if again.Selection.DropReasons["system_budget"] != 1 {
		t.Fatal("Snapshot exposed mutable holder state")
	}
}
