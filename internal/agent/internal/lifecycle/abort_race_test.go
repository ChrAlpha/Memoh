package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func TestAbortReconciliationPrefersResumedRunSnapshotOverPausedMetadata(t *testing.T) {
	staleMetadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: contextfrag.LifecycleSnapshot{Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalSnapshot, err := json.Marshal(contextfrag.LifecycleSnapshot{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	queries := agenttest.NewAbortedLifecycleQueries(t)
	queries.Existing = &sqlc.GetContextLifecycleByRunIDRow{Snapshot: finalSnapshot}
	queries.ExistingAfter = 2
	queries.Metadata = staleMetadata
	service := New(Deps{Store: queries, Runs: queries, Decisions: queries})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	service.ReconcileAborted(
		ctx,
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)

	upserts := queries.RecordedUpserts()
	if len(upserts) != 1 || !bytes.Equal(upserts[0].Snapshot, finalSnapshot) {
		t.Fatalf("aborted upserts = %#v, want resumed snapshot %s", upserts, finalSnapshot)
	}
	if bytes.Contains(upserts[0].Snapshot, []byte(`"version":1`)) {
		t.Fatalf("aborted lifecycle used stale paused metadata: %s", upserts[0].Snapshot)
	}
}

func TestAbortReconciliationRechecksPendingDecisionBeforeMetadataFallback(t *testing.T) {
	staleMetadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: contextfrag.LifecycleSnapshot{Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalSnapshot, err := json.Marshal(contextfrag.LifecycleSnapshot{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	queries := agenttest.NewAbortedLifecycleQueries(t)
	queries.Existing = &sqlc.GetContextLifecycleByRunIDRow{Snapshot: finalSnapshot}
	queries.ExistingAfter = 4
	queries.Metadata = staleMetadata
	queries.Pending = true
	queries.PendingUntil = 1
	service := New(Deps{Store: queries, Runs: queries, Decisions: queries})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	service.ReconcileAborted(
		ctx,
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)

	upserts := queries.RecordedUpserts()
	if len(upserts) != 1 || !bytes.Equal(upserts[0].Snapshot, finalSnapshot) {
		t.Fatalf("aborted upserts = %#v, want resumed snapshot %s", upserts, finalSnapshot)
	}
	pendingReads := queries.PendingReads()
	if pendingReads < 2 {
		t.Fatalf("pending decision reads = %d, want a recheck", pendingReads)
	}
}
