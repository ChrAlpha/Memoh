package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
)

func lifecycleRow(t *testing.T, role string, at time.Time, snapshot *contextfrag.LifecycleSnapshot) sqlc.ListRecentAssistantMessagesBySessionRow {
	t.Helper()
	metadata := map[string]any{}
	if snapshot != nil {
		metadata[contextfrag.MetadataContextLifecycleKey] = snapshot
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return sqlc.ListRecentAssistantMessagesBySessionRow{
		ID:        pgtype.UUID{Bytes: [16]byte{byte(at.Unix() % 256)}, Valid: true}, //nolint:gosec // test fixture
		Role:      role,
		Metadata:  raw,
		CreatedAt: pgtype.Timestamptz{Time: at, Valid: true},
	}
}

func TestLifecycleTurnsFromRowsFiltersAndOrders(t *testing.T) {
	t.Parallel()

	base := time.Unix(1000, 0).UTC()
	rows := []sqlc.ListRecentAssistantMessagesBySessionRow{
		lifecycleRow(t, "assistant", base.Add(3*time.Minute), &contextfrag.LifecycleSnapshot{Version: 1, FinalInputHash: "turn-2"}),
		lifecycleRow(t, "assistant", base.Add(2*time.Minute), nil),
		lifecycleRow(t, "assistant", base.Add(time.Minute), &contextfrag.LifecycleSnapshot{Version: 1, FinalInputHash: "turn-1"}),
	}

	turns := lifecycleTurnsFromRows(rows, 10)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (rows with a lifecycle snapshot only)", len(turns))
	}
	if turns[0].Snapshot.FinalInputHash != "turn-2" || turns[1].Snapshot.FinalInputHash != "turn-1" {
		t.Fatalf("turns must be newest-first: %q then %q", turns[0].Snapshot.FinalInputHash, turns[1].Snapshot.FinalInputHash)
	}

	limited := lifecycleTurnsFromRows(rows, 1)
	if len(limited) != 1 || limited[0].Snapshot.FinalInputHash != "turn-2" {
		t.Fatalf("limit must keep the newest turns: %#v", limited)
	}
}

func TestAggregateContextLifecycle(t *testing.T) {
	t.Parallel()

	turns := []ContextLifecycleTurn{
		{Snapshot: contextfrag.LifecycleSnapshot{
			CacheReadTokens:  100,
			CacheWriteTokens: 10,
			CacheComparison:  &contextfrag.CacheComparison{Outcome: contextfrag.CacheOutcomeHit},
			Selection:        contextfrag.SelectionTrace{DropReasons: map[string]int{"can_drop": 3}},
			Mutations: []contextfrag.MutationRecord{
				{Kind: contextfrag.MutationBeforeModelCallHook},
				{Kind: contextfrag.MutationMidTaskPrune},
			},
		}},
		{Snapshot: contextfrag.LifecycleSnapshot{
			CacheReadTokens: 0,
			CacheComparison: &contextfrag.CacheComparison{Outcome: contextfrag.CacheOutcomeMissSamePrefix},
			Selection:       contextfrag.SelectionTrace{DropReasons: map[string]int{"can_drop": 1, "trust_gate:external_in_system_slot": 1}},
		}},
		{Snapshot: contextfrag.LifecycleSnapshot{
			CacheComparison: &contextfrag.CacheComparison{Outcome: contextfrag.CacheOutcomeFirstObservation},
		}},
	}

	agg := aggregateContextLifecycle(turns)
	if agg.Turns != 3 {
		t.Fatalf("turns = %d, want 3", agg.Turns)
	}
	if agg.CacheOutcomes[contextfrag.CacheOutcomeHit] != 1 || agg.CacheOutcomes[contextfrag.CacheOutcomeMissSamePrefix] != 1 {
		t.Fatalf("cache outcomes = %#v", agg.CacheOutcomes)
	}
	// Hit rate counts only comparable turns (first observations excluded).
	if agg.CacheHitRate != 50 {
		t.Fatalf("hit rate = %v, want 50", agg.CacheHitRate)
	}
	if agg.TotalCacheReadTokens != 100 || agg.TotalCacheWriteTokens != 10 {
		t.Fatalf("cache totals = %d/%d", agg.TotalCacheReadTokens, agg.TotalCacheWriteTokens)
	}
	if agg.DropReasons["can_drop"] != 4 || agg.DropReasons["trust_gate:external_in_system_slot"] != 1 {
		t.Fatalf("drop reasons = %#v", agg.DropReasons)
	}
	if agg.MutationKinds["before_model_call_hook"] != 1 || agg.MutationKinds["mid_task_prune"] != 1 {
		t.Fatalf("mutation kinds = %#v", agg.MutationKinds)
	}
}
