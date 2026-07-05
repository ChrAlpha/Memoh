package contextview

import (
	"errors"
	"testing"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func TestProviderViewFallbackKeepsLedgerAndLifecycleVisible(t *testing.T) {
	ledger := contextfrag.NewMutationLedger()
	holder := contextfrag.NewLifecycleHolder()
	cfg := agentpkg.RunConfig{System: "sys", Query: "hi", ContextLifecycle: holder}

	out := providerViewFallback(nil, cfg, ledger, "build_error",
		"context view build failed; using legacy assembly", errors.New("boom"))

	if out.ContextMutations != ledger {
		t.Fatal("fallback dropped the mutation ledger")
	}
	records := ledger.Records()
	if len(records) != 1 || records[0].Kind != contextfrag.MutationContextViewFallback {
		t.Fatalf("records = %+v, want one %s", records, contextfrag.MutationContextViewFallback)
	}
	if records[0].Detail != "build_error" {
		t.Fatalf("record detail = %q, want reason", records[0].Detail)
	}
	if out.ContextManifest.Mutations != ledger {
		t.Fatal("fallback manifest does not carry the ledger")
	}
	snapshot, ok := holder.Snapshot()
	if !ok {
		t.Fatal("lifecycle holder did not receive the fallback manifest")
	}
	if len(snapshot.Mutations) != 1 {
		t.Fatalf("snapshot mutations = %d, want the fallback record", len(snapshot.Mutations))
	}
	if len(out.Messages) == 0 {
		t.Fatal("legacy materialization did not append the current query")
	}
}
