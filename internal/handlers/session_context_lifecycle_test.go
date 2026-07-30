package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/bots"
	session "github.com/memohai/memoh/internal/chat/thread"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

type contextLifecycleQueryStub struct {
	dbstore.Queries
	bot             sqlc.GetBotByIDRow
	session         sqlc.BotSession
	lifecycleRows   []sqlc.ListRecentContextLifecyclesBySessionRow
	lifecycleErr    error
	lifecycleParams []sqlc.ListRecentContextLifecyclesBySessionParams
	legacyRows      []sqlc.ListRecentAssistantMessagesBySessionRow
	legacyErr       error
	legacyParams    []sqlc.ListRecentAssistantMessagesBySessionParams
}

func (q *contextLifecycleQueryStub) GetBotByID(_ context.Context, _ pgtype.UUID) (sqlc.GetBotByIDRow, error) {
	return q.bot, nil
}

func (q *contextLifecycleQueryStub) GetSessionByID(_ context.Context, _ pgtype.UUID) (sqlc.BotSession, error) {
	return q.session, nil
}

func (q *contextLifecycleQueryStub) ListRecentContextLifecyclesBySession(
	_ context.Context,
	arg sqlc.ListRecentContextLifecyclesBySessionParams,
) ([]sqlc.ListRecentContextLifecyclesBySessionRow, error) {
	q.lifecycleParams = append(q.lifecycleParams, arg)
	return q.lifecycleRows, q.lifecycleErr
}

func (q *contextLifecycleQueryStub) ListRecentAssistantMessagesBySession(
	_ context.Context,
	arg sqlc.ListRecentAssistantMessagesBySessionParams,
) ([]sqlc.ListRecentAssistantMessagesBySessionRow, error) {
	q.legacyParams = append(q.legacyParams, arg)
	return q.legacyRows, q.legacyErr
}

func lifecycleSnapshotJSON(t *testing.T, snapshot contextfrag.LifecycleSnapshot) []byte {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal lifecycle snapshot: %v", err)
	}
	return raw
}

func TestGetSessionContextLifecycleReturnsFailedRunWithoutAssistantMessage(t *testing.T) {
	t.Parallel()

	const (
		botID                = "11111111-1111-1111-1111-111111111111"
		sessionID            = "22222222-2222-2222-2222-222222222222"
		failedRunID          = "33333333-3333-3333-3333-333333333333"
		completedRunID       = "44444444-4444-4444-4444-444444444444"
		assistantMessageID   = "55555555-5555-5555-5555-555555555555"
		budgetErrorCode      = "context.budget_unsatisfied"
		failedFinalInputHash = "failed-before-assistant"
	)
	createdAt := time.Unix(1000, 0).UTC()
	queries := &contextLifecycleQueryStub{
		bot: testBotRow(botID, map[string]any{}),
		session: sqlc.BotSession{
			ID:          testUUID(sessionID),
			BotID:       testUUID(botID),
			Type:        session.TypeChat,
			SessionMode: session.TypeChat,
			RuntimeType: session.RuntimeModel,
		},
		lifecycleRows: []sqlc.ListRecentContextLifecyclesBySessionRow{
			{
				RunID:     testUUID(failedRunID),
				Status:    "failed_budget",
				ErrorCode: pgtype.Text{String: budgetErrorCode, Valid: true},
				CreatedAt: pgtype.Timestamptz{Time: createdAt.Add(time.Minute), Valid: true},
				Snapshot: lifecycleSnapshotJSON(t, contextfrag.LifecycleSnapshot{
					Version:        1,
					FinalInputHash: failedFinalInputHash,
				}),
			},
			{
				RunID:     testUUID(completedRunID),
				Status:    "completed",
				CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
				Snapshot: lifecycleSnapshotJSON(t, contextfrag.LifecycleSnapshot{
					Version:            1,
					AssistantMessageID: assistantMessageID,
				}),
			},
		},
	}
	handler := NewSessionInfoHandler(
		slog.New(slog.DiscardHandler),
		queries,
		bots.NewService(nil, queries),
		newTestAdminAccountService("admin"),
		nil,
		nil,
	)

	e := echo.New()
	req := httptest.NewRequest(
		http.MethodGet,
		"/bots/"+botID+"/sessions/"+sessionID+"/context-lifecycle?limit=2",
		nil,
	)
	rec := httptest.NewRecorder()
	ctx := testAuthContext(e, req, rec, "user-1")
	ctx.SetPath("/bots/:bot_id/sessions/:session_id/context-lifecycle")
	ctx.SetParamNames("bot_id", "session_id")
	ctx.SetParamValues(botID, sessionID)

	if err := handler.GetSessionContextLifecycle(ctx); err != nil {
		t.Fatalf("GetSessionContextLifecycle() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var response ContextLifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(response.Turns))
	}
	failed := response.Turns[0]
	if failed.RunID != failedRunID || failed.Status != "failed_budget" || failed.ErrorCode != budgetErrorCode ||
		failed.Snapshot.FinalInputHash != failedFinalInputHash || failed.AssistantMessageID != "" {
		t.Fatalf("failed run response = %#v", failed)
	}
	completed := response.Turns[1]
	if completed.RunID != completedRunID || completed.AssistantMessageID != assistantMessageID {
		t.Fatalf("completed run response = %#v, want assistant association", completed)
	}
	if strings.Contains(rec.Body.String(), `"message_id"`) {
		t.Fatalf("response must not use assistant messages as lifecycle identity: %s", rec.Body.String())
	}
	if len(queries.legacyParams) != 0 {
		t.Fatalf("legacy query calls = %d, want 0", len(queries.legacyParams))
	}
	if len(queries.lifecycleParams) != 1 || queries.lifecycleParams[0].MaxCount != 2 {
		t.Fatalf("run query params = %#v, want limit 2", queries.lifecycleParams)
	}
}

func TestLoadContextLifecycleTurnsPrefersRunRowsWithoutAssistantMessage(t *testing.T) {
	t.Parallel()

	runID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	sessionID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	createdAt := time.Unix(1000, 0).UTC()
	queries := &contextLifecycleQueryStub{
		lifecycleRows: []sqlc.ListRecentContextLifecyclesBySessionRow{{
			RunID:     runID,
			Status:    "failed_budget",
			ErrorCode: pgtype.Text{String: "context.budget_unsatisfied", Valid: true},
			CreatedAt: pgtype.Timestamptz{Time: createdAt, Valid: true},
			Snapshot: lifecycleSnapshotJSON(t, contextfrag.LifecycleSnapshot{
				Version:        1,
				FinalInputHash: "failed-before-assistant",
			}),
		}},
	}

	turns, err := loadContextLifecycleTurns(context.Background(), queries, sessionID, 7)
	if err != nil {
		t.Fatalf("load context lifecycle turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want failed run without an assistant message", len(turns))
	}
	turn := turns[0]
	if turn.RunID != runID.String() || turn.Status != "failed_budget" ||
		turn.ErrorCode != "context.budget_unsatisfied" || !turn.CreatedAt.Equal(createdAt) {
		t.Fatalf("turn = %#v, want run-keyed failed_budget lifecycle", turn)
	}
	if turn.Snapshot.FinalInputHash != "failed-before-assistant" {
		t.Fatalf("snapshot = %#v, want persisted run snapshot", turn.Snapshot)
	}
	if len(queries.legacyParams) != 0 {
		t.Fatalf("legacy query calls = %d, want 0 when run rows exist", len(queries.legacyParams))
	}
	if len(queries.lifecycleParams) != 1 || queries.lifecycleParams[0].MaxCount != 7 {
		t.Fatalf("run query params = %#v, want one call with limit 7", queries.lifecycleParams)
	}
}

func TestLoadContextLifecycleTurnsPreservesRunOrderingAndLimit(t *testing.T) {
	t.Parallel()

	rows := make([]sqlc.ListRecentContextLifecyclesBySessionRow, 0, 3)
	for i := byte(1); i <= 3; i++ {
		rows = append(rows, sqlc.ListRecentContextLifecyclesBySessionRow{
			RunID:     pgtype.UUID{Bytes: [16]byte{i}, Valid: true},
			Status:    "completed",
			CreatedAt: pgtype.Timestamptz{Time: time.Unix(int64(100-i), 0).UTC(), Valid: true},
			Snapshot: lifecycleSnapshotJSON(t, contextfrag.LifecycleSnapshot{
				Version:        1,
				FinalInputHash: string(rune('a' + i - 1)),
			}),
		})
	}
	queries := &contextLifecycleQueryStub{lifecycleRows: rows}

	turns, err := loadContextLifecycleTurns(
		context.Background(),
		queries,
		pgtype.UUID{Bytes: [16]byte{9}, Valid: true},
		2,
	)
	if err != nil {
		t.Fatalf("load context lifecycle turns: %v", err)
	}
	if len(turns) != 2 || turns[0].Snapshot.FinalInputHash != "a" || turns[1].Snapshot.FinalInputHash != "b" {
		t.Fatalf("turns = %#v, want query order bounded to two rows", turns)
	}
}

func TestLoadContextLifecycleTurnsFallsBackOnlyWhenRunRowsDoNotExist(t *testing.T) {
	t.Parallel()

	runID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	createdAt := time.Unix(1000, 0).UTC()
	queries := &contextLifecycleQueryStub{
		legacyRows: []sqlc.ListRecentAssistantMessagesBySessionRow{
			lifecycleRow(t, runID, createdAt, &contextfrag.LifecycleSnapshot{
				Version:        1,
				FinalInputHash: "legacy",
			}),
		},
	}

	turns, err := loadContextLifecycleTurns(
		context.Background(),
		queries,
		pgtype.UUID{Bytes: [16]byte{5}, Valid: true},
		1,
	)
	if err != nil {
		t.Fatalf("load context lifecycle turns: %v", err)
	}
	if len(turns) != 1 || turns[0].RunID != runID.String() || turns[0].Status != "" ||
		turns[0].AssistantMessageID == "" || turns[0].Snapshot.FinalInputHash != "legacy" {
		t.Fatalf("turns = %#v, want legacy assistant metadata fallback", turns)
	}
	if len(queries.lifecycleParams) != 1 || len(queries.legacyParams) != 1 {
		t.Fatalf("query calls = run:%d legacy:%d, want one each", len(queries.lifecycleParams), len(queries.legacyParams))
	}
}

func TestLoadContextLifecycleTurnsDoesNotMaskRunQueryFailure(t *testing.T) {
	t.Parallel()

	queries := &contextLifecycleQueryStub{lifecycleErr: errors.New("run store unavailable")}
	_, err := loadContextLifecycleTurns(
		context.Background(),
		queries,
		pgtype.UUID{Bytes: [16]byte{6}, Valid: true},
		1,
	)
	if err == nil {
		t.Fatal("expected run-table query failure")
	}
	if len(queries.legacyParams) != 0 {
		t.Fatalf("legacy query calls = %d, want no fallback on query failure", len(queries.legacyParams))
	}
}

func lifecycleRow(t *testing.T, runID pgtype.UUID, at time.Time, snapshot *contextfrag.LifecycleSnapshot) sqlc.ListRecentAssistantMessagesBySessionRow {
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
		RunID:     runID,
		Role:      "assistant",
		Metadata:  raw,
		CreatedAt: pgtype.Timestamptz{Time: at, Valid: true},
	}
}

func TestLegacyLifecycleTurnsFromRowsFiltersAndOrders(t *testing.T) {
	t.Parallel()

	base := time.Unix(1000, 0).UTC()
	rows := []sqlc.ListRecentAssistantMessagesBySessionRow{
		lifecycleRow(t, pgtype.UUID{Bytes: [16]byte{3}, Valid: true}, base.Add(3*time.Minute), &contextfrag.LifecycleSnapshot{Version: 1, FinalInputHash: "turn-2"}),
		lifecycleRow(t, pgtype.UUID{Bytes: [16]byte{2}, Valid: true}, base.Add(2*time.Minute), nil),
		lifecycleRow(t, pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, base.Add(time.Minute), &contextfrag.LifecycleSnapshot{Version: 1, FinalInputHash: "turn-1"}),
	}

	turns := legacyLifecycleTurnsFromRows(rows, 10)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2 (rows with a lifecycle snapshot only)", len(turns))
	}
	if turns[0].Snapshot.FinalInputHash != "turn-2" || turns[1].Snapshot.FinalInputHash != "turn-1" {
		t.Fatalf("turns must be newest-first: %q then %q", turns[0].Snapshot.FinalInputHash, turns[1].Snapshot.FinalInputHash)
	}

	limited := legacyLifecycleTurnsFromRows(rows, 1)
	if len(limited) != 1 || limited[0].Snapshot.FinalInputHash != "turn-2" {
		t.Fatalf("limit must keep the newest turns: %#v", limited)
	}
}

func TestLegacyLifecycleTurnsFromRowsSupportsLegacyAndMemoryOnlySnapshots(t *testing.T) {
	t.Parallel()

	base := time.Unix(1000, 0).UTC()
	rows := []sqlc.ListRecentAssistantMessagesBySessionRow{
		lifecycleRow(t, pgtype.UUID{Bytes: [16]byte{2}, Valid: true}, base.Add(time.Minute), &contextfrag.LifecycleSnapshot{
			Version: 1,
			MemoryRecall: &contextfrag.MemoryRecallTrace{
				ProviderID: "provider-1",
				CacheState: "miss",
				Result: contextfrag.MemoryRecallResultTrace{
					Count: 1,
					Refs:  []string{"memory-1"},
				},
			},
		}),
		lifecycleRow(t, pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, base, &contextfrag.LifecycleSnapshot{
			Version:        1,
			FinalInputHash: "legacy-snapshot",
		}),
	}

	turns := legacyLifecycleTurnsFromRows(rows, 10)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want memory and legacy snapshots", len(turns))
	}
	if turns[0].Snapshot.MemoryRecall == nil || turns[0].Snapshot.MemoryRecall.ProviderID != "provider-1" ||
		turns[0].Snapshot.MemoryRecall.Result.Count != 1 {
		t.Fatalf("memory-only snapshot = %#v", turns[0].Snapshot)
	}
	if turns[1].Snapshot.MemoryRecall != nil || turns[1].Snapshot.FinalInputHash != "legacy-snapshot" {
		t.Fatalf("legacy snapshot changed compatibility semantics: %#v", turns[1].Snapshot)
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
