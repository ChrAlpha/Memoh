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
	if ledger.Records() != nil {
		t.Fatal("nil ledger must be inert")
	}
}

func TestMutationLedgerRecordsInOrder(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.Record(MutationBackgroundSummary, "bytes=10")
	ledger.Record(MutationContextSwitch, "mode=fold")

	records := ledger.Records()
	if len(records) != 2 ||
		records[0].Kind != MutationBackgroundSummary ||
		records[1].Kind != MutationContextSwitch {
		t.Fatalf("records = %#v", records)
	}
}

func TestMutationLedgerAdvanceAttemptReturnsNewAttemptNumber(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	if got := ledger.AdvanceAttempt(); got != 1 {
		t.Fatalf("AdvanceAttempt() = %d, want 1", got)
	}
	if got := ledger.AdvanceAttempt(); got != 2 {
		t.Fatalf("AdvanceAttempt() = %d, want 2", got)
	}
	var nilLedger *MutationLedger
	if got := nilLedger.AdvanceAttempt(); got != 0 {
		t.Fatalf("nil ledger AdvanceAttempt() = %d, want 0", got)
	}
}

func TestMutationLedgerAppendStepSnapshotStampsCurrentAttempt(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0})
	ledger.AdvanceAttempt()
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0})

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

func TestMutationLedgerStepAttemptModelNilSafe(t *testing.T) {
	t.Parallel()

	var ledger *MutationLedger
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0})
	ledger.AdvanceAttempt()
	ledger.RecordCacheUsage(CacheUsageRecord{StepIndex: 0})
	ledger.SetModelInfo("m", "c")
	if ledger.StepSnapshots() != nil || ledger.CacheUsageRecords() != nil {
		t.Fatal("nil ledger step/cache records must be nil")
	}
	if model, clientType := ledger.ModelInfo(); model != "" || clientType != "" {
		t.Fatalf("nil ledger ModelInfo() = (%q, %q), want empty", model, clientType)
	}
}

func TestMutationLedgerMarshalJSONIncludesAttemptAudit(t *testing.T) {
	t.Parallel()

	ledger := NewMutationLedger()
	ledger.Record(MutationContextSwitch, "mode=fold trigger=in_run step=3")
	ledger.SetModelInfo("claude-x", "anthropic-messages")
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 0})
	ledger.RecordCacheUsage(CacheUsageRecord{StepIndex: 0, CacheReadTokens: 11})
	ledger.AdvanceAttempt()
	ledger.AppendStepSnapshot(StepSnapshot{StepIndex: 1})

	raw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	for _, want := range []string{
		"context_switch", "mode=fold trigger=in_run step=3",
		`"model":"claude-x"`, `"client_type":"anthropic-messages"`,
		`"step_index":0`, `"cache_read_tokens":11`,
		`"step_index":1`, `"attempt":1`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("ledger JSON missing %q:\n%s", want, raw)
		}
	}
}
