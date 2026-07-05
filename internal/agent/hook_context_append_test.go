package agent

import (
	"testing"

	"github.com/memohai/memoh/internal/contextfrag"
)

func TestApplyBeforeModelCallAppendContextRecordsMutation(t *testing.T) {
	ledger := contextfrag.NewMutationLedger()
	cfg := RunConfig{ContextMutations: ledger}
	out := applyBeforeModelCallAppendContext(cfg, "extra guidance")
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	records := ledger.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationBeforeModelCallHook {
		t.Fatalf("records = %+v, want one %s", records, contextfrag.MutationBeforeModelCallHook)
	}
}

func TestApplyBeforeModelCallAppendContextEmptyIsNoop(t *testing.T) {
	ledger := contextfrag.NewMutationLedger()
	cfg := RunConfig{ContextMutations: ledger}
	out := applyBeforeModelCallAppendContext(cfg, "  ")
	if len(out.Messages) != 0 || len(ledger.Records()) != 0 {
		t.Fatalf("expected noop, got messages=%d records=%d", len(out.Messages), len(ledger.Records()))
	}
}
