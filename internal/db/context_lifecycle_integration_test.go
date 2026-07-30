//go:build integration

package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	dbpkg "github.com/memohai/memoh/internal/db"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/team"
)

func TestContextLifecycleMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := freshMigratedDB(t)
	dsn := teamMigrationDSN(t)

	assertContextLifecycleSchema(t, ctx, pool, true)
	stepDown(t, dsn, countMigrationsFrom(t, "0124_context_lifecycles.up.sql"))
	assertContextLifecycleSchema(t, ctx, pool, false)
	stepUp(t, dsn, countMigrationsFrom(t, "0124_context_lifecycles.up.sql"))
	assertContextLifecycleSchema(t, ctx, pool, true)
}

func TestCanonicalInitContainsContextLifecycles(t *testing.T) {
	ctx := context.Background()
	dsn := teamMigrationDSN(t)
	pool := resetToEmpty(t)
	applyCanonicalInitOnly(t, dsn)

	assertContextLifecycleSchema(t, ctx, pool, true)
}

func TestContextLifecycleQueriesRoundTripContentLight(t *testing.T) {
	ctx := context.Background()
	pool := freshMigratedDB(t)
	dsn := teamMigrationDSN(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire database connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('memoh.team_id', $1, false)", team.DefaultTeamID); err != nil {
		t.Fatalf("bind default team: %v", err)
	}

	const (
		botID     = "00000000-0000-0000-0000-00000000b501"
		sessionID = "00000000-0000-0000-0000-00000000c501"
		runID     = "00000000-0000-0000-0000-00000000d501"
		secret    = "private prompt text must never be persisted"
	)
	if _, err := conn.Exec(ctx, `
WITH principal AS (
  INSERT INTO users (username, is_active, metadata)
  VALUES ('context-lifecycle-owner', true, '{}')
  RETURNING id
), membership AS (
  INSERT INTO team_members (team_id, user_id)
  SELECT $1, principal.id FROM principal
  RETURNING user_id
), bot AS (
  INSERT INTO bots (id, team_id, owner_user_id, name, status, metadata)
  SELECT $2, $1, membership.user_id, 'context-lifecycle-bot', 'ready', '{}' FROM membership
  RETURNING id
)
INSERT INTO bot_sessions (id, team_id, bot_id, channel_type, title, metadata)
SELECT $3, $1, bot.id, 'local', 'context lifecycle', '{}' FROM bot
`, team.DefaultTeamID, botID, sessionID); err != nil {
		t.Fatalf("seed context lifecycle owner: %v", err)
	}

	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(secret)))
	snapshot := contextfrag.LifecycleSnapshot{
		Version: 1,
		Counts: contextfrag.ManifestCounts{
			Fragments:     1,
			TextBytes:     len(secret),
			TokenEstimate: 9,
		},
		SelectionDecisions: []contextfrag.SelectionDecision{{
			ID: "system-policy",
			Ref: contextfrag.ContextRef{
				Namespace:   "native-system",
				ID:          "policy",
				ContentHash: contentHash,
				Schema:      "context-frag/v1",
			},
			Slot:          contextfrag.SlotSystem,
			Source:        "embedded-template",
			Decision:      contextfrag.DecisionSelected,
			TokenEstimate: 9,
			TextBytes:     len(secret),
			CacheClass:    contextfrag.CacheStable,
			RetentionTier: contextfrag.RetentionRequired,
		}},
		BudgetPlan: &contextfrag.ContextBudgetPlan{
			Window:           32_768,
			OutputReserve:    4_096,
			SystemBudget:     8_192,
			ActualSystemCost: 9,
			HistoryBudget:    20_471,
		},
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal lifecycle snapshot: %v", err)
	}
	if strings.Contains(string(snapshotJSON), secret) {
		t.Fatal("content-light lifecycle snapshot contains raw prompt text before persistence")
	}

	parsedRunID := mustParseUUID(t, runID)
	parsedBotID := mustParseUUID(t, botID)
	parsedSessionID := mustParseUUID(t, sessionID)
	queries := sqlc.New(conn)
	created, err := queries.CreateContextLifecycle(ctx, sqlc.CreateContextLifecycleParams{
		RunID:     parsedRunID,
		BotID:     parsedBotID,
		SessionID: parsedSessionID,
		Status:    "failed_budget",
		ErrorCode: pgtype.Text{String: "context_budget_unsatisfied", Valid: true},
		Snapshot:  snapshotJSON,
	})
	if err != nil {
		t.Fatalf("create context lifecycle: %v", err)
	}
	if created.RunID != parsedRunID || created.Status != "failed_budget" {
		t.Fatalf("created lifecycle identity = (%v, %q), want (%v, failed_budget)", created.RunID, created.Status, parsedRunID)
	}

	got, err := queries.GetContextLifecycleByRunID(ctx, parsedRunID)
	if err != nil {
		t.Fatalf("get context lifecycle: %v", err)
	}
	var roundTripped contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(got.Snapshot, &roundTripped); err != nil {
		t.Fatalf("unmarshal lifecycle snapshot: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, snapshot) {
		t.Fatalf("round-tripped lifecycle snapshot = %#v, want %#v", roundTripped, snapshot)
	}
	if strings.Contains(string(got.Snapshot), secret) {
		t.Fatal("persisted lifecycle snapshot contains raw prompt text")
	}

	const pausedRunID = "00000000-0000-0000-0000-00000000d502"
	pausedMetadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: snapshot,
	})
	if err != nil {
		t.Fatalf("marshal paused lifecycle metadata: %v", err)
	}
	parsedPausedRunID := mustParseUUID(t, pausedRunID)
	if _, err := conn.Exec(ctx, `
INSERT INTO bot_history_messages (bot_id, session_id, role, content, metadata, run_id)
VALUES ($1, $2, 'assistant', '{}'::jsonb, $3, $4)
`, botID, sessionID, pausedMetadata, pausedRunID); err != nil {
		t.Fatalf("seed paused assistant lifecycle: %v", err)
	}
	pausedRaw, err := queries.GetLatestAssistantContextLifecycleMetadataByRunID(ctx, parsedPausedRunID)
	if err != nil {
		t.Fatalf("get paused assistant lifecycle metadata: %v", err)
	}
	pausedSnapshot, ok := contextfrag.LifecycleSnapshotFromMetadata(pausedRaw)
	if !ok || !reflect.DeepEqual(pausedSnapshot, snapshot) {
		t.Fatalf("paused lifecycle snapshot = %#v, %t; want %#v", pausedSnapshot, ok, snapshot)
	}

	recent, err := queries.ListRecentContextLifecyclesBySession(ctx, sqlc.ListRecentContextLifecyclesBySessionParams{
		SessionID: parsedSessionID,
		MaxCount:  10,
	})
	if err != nil {
		t.Fatalf("list context lifecycles: %v", err)
	}
	if len(recent) != 1 || recent[0].RunID != parsedRunID || recent[0].Status != "failed_budget" {
		t.Fatalf("recent context lifecycles = %#v, want one failed_budget row for %s", recent, runID)
	}

	if _, err := queries.CreateContextLifecycle(ctx, sqlc.CreateContextLifecycleParams{
		RunID:     parsedRunID,
		BotID:     parsedBotID,
		SessionID: parsedSessionID,
		Status:    "completed",
		Snapshot:  []byte(`{}`),
	}); sqlState(err) != "23505" {
		t.Fatalf("duplicate run lifecycle SQLSTATE = %q, want 23505", sqlState(err))
	}

	replacementSnapshot := []byte(`{"version":999}`)
	aborted, err := queries.UpsertAbortedContextLifecycle(ctx, sqlc.UpsertAbortedContextLifecycleParams{
		RunID:     parsedRunID,
		BotID:     parsedBotID,
		SessionID: parsedSessionID,
		Snapshot:  replacementSnapshot,
	})
	if err != nil {
		t.Fatalf("upsert existing aborted context lifecycle: %v", err)
	}
	if aborted.Status != "aborted" || aborted.ErrorCode.Valid {
		t.Fatalf("aborted lifecycle terminal = (%q, %#v), want aborted with no error code", aborted.Status, aborted.ErrorCode)
	}
	if !reflect.DeepEqual(aborted.Snapshot, created.Snapshot) {
		t.Fatalf("aborted lifecycle replaced existing snapshot = %s, want %s", aborted.Snapshot, created.Snapshot)
	}
	if aborted.CreatedAt != created.CreatedAt {
		t.Fatalf("aborted lifecycle changed created_at = %#v, want %#v", aborted.CreatedAt, created.CreatedAt)
	}

	const abortedRunID = "00000000-0000-0000-0000-00000000d503"
	parsedAbortedRunID := mustParseUUID(t, abortedRunID)
	insertedAborted, err := queries.UpsertAbortedContextLifecycle(ctx, sqlc.UpsertAbortedContextLifecycleParams{
		RunID:     parsedAbortedRunID,
		BotID:     parsedBotID,
		SessionID: parsedSessionID,
		Snapshot:  replacementSnapshot,
	})
	if err != nil {
		t.Fatalf("insert aborted context lifecycle: %v", err)
	}
	var insertedSnapshot map[string]any
	if err := json.Unmarshal(insertedAborted.Snapshot, &insertedSnapshot); err != nil {
		t.Fatalf("unmarshal inserted aborted snapshot: %v", err)
	}
	if insertedAborted.Status != "aborted" || insertedAborted.ErrorCode.Valid ||
		insertedSnapshot["version"] != float64(999) {
		t.Fatalf("inserted aborted lifecycle = %#v", insertedAborted)
	}
	authoritativeSnapshot := []byte(`{"version":1000}`)
	convergedAborted, err := queries.UpdateAbortedContextLifecycleSnapshot(
		ctx,
		sqlc.UpdateAbortedContextLifecycleSnapshotParams{
			Snapshot:  authoritativeSnapshot,
			RunID:     parsedAbortedRunID,
			BotID:     parsedBotID,
			SessionID: parsedSessionID,
		},
	)
	if err != nil {
		t.Fatalf("replace recovered aborted snapshot: %v", err)
	}
	var convergedSnapshot map[string]any
	if err := json.Unmarshal(convergedAborted.Snapshot, &convergedSnapshot); err != nil {
		t.Fatalf("unmarshal converged aborted snapshot: %v", err)
	}
	if convergedAborted.Status != "aborted" || convergedAborted.ErrorCode.Valid ||
		convergedSnapshot["version"] != float64(1000) {
		t.Fatalf("converged aborted lifecycle = %#v", convergedAborted)
	}
	if convergedAborted.CreatedAt != insertedAborted.CreatedAt {
		t.Fatalf("authoritative snapshot update changed created_at = %#v, want %#v", convergedAborted.CreatedAt, insertedAborted.CreatedAt)
	}

	const teamTwo = "00000000-0000-0000-0000-0000000000f2"
	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, slug) VALUES ($1, 'context-lifecycle-team-two')`, teamTwo); err != nil {
		t.Fatalf("seed second team: %v", err)
	}
	rc := rlsConn(t, pool, dsn)
	if _, err := rc.Exec(ctx, "SELECT set_config('memoh.team_id', $1, false)", teamTwo); err != nil {
		t.Fatalf("bind second team: %v", err)
	}
	var visible int
	if err := rc.QueryRow(ctx, "SELECT count(*) FROM context_lifecycles").Scan(&visible); err != nil {
		t.Fatalf("count second-team context lifecycles: %v", err)
	}
	if visible != 0 {
		t.Fatalf("second team saw %d context lifecycle rows, want 0", visible)
	}
	_, crossTeamErr := rc.Exec(ctx, `
INSERT INTO context_lifecycles (team_id, run_id, bot_id, session_id, status, snapshot)
VALUES ($1, gen_random_uuid(), $2, $3, 'completed', '{}'::jsonb)
`, team.DefaultTeamID, botID, sessionID)
	if sqlState(crossTeamErr) != "42501" {
		t.Fatalf("cross-team lifecycle insert SQLSTATE = %q, want 42501", sqlState(crossTeamErr))
	}
}

func assertContextLifecycleSchema(t *testing.T, ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, want bool) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(ctx, "SELECT to_regclass('public.context_lifecycles') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("inspect context lifecycle table: %v", err)
	}
	if exists != want {
		t.Fatalf("context_lifecycles exists = %t, want %t", exists, want)
	}
	if !want {
		return
	}
	var (
		indexExists    bool
		rlsEnabled     bool
		rlsForced      bool
		statusValues   string
		sessionRunFKs  int
		tenantFKs      int
		tenantKeyFound bool
	)
	if err := db.QueryRow(ctx, `
SELECT
  to_regclass('public.idx_context_lifecycles_session_recent') IS NOT NULL,
  c.relrowsecurity,
  c.relforcerowsecurity,
  pg_get_constraintdef(status_con.oid),
  (SELECT count(*) FROM pg_constraint con
    WHERE con.conrelid = 'public.context_lifecycles'::regclass
      AND con.contype = 'f'
      AND con.confrelid = 'public.session_runs'::regclass),
  (SELECT count(*) FROM pg_constraint con
    WHERE con.conrelid = 'public.context_lifecycles'::regclass
      AND con.contype = 'f'
      AND con.confrelid IN ('public.bots'::regclass, 'public.bot_sessions'::regclass)),
  EXISTS (
    SELECT 1 FROM pg_constraint con
    WHERE con.conrelid = 'public.context_lifecycles'::regclass
      AND con.contype = 'u'
      AND pg_get_constraintdef(con.oid) = 'UNIQUE (team_id, run_id)'
  )
FROM pg_class c
JOIN pg_constraint status_con
  ON status_con.conrelid = c.oid
 AND status_con.conname = 'context_lifecycles_status_check'
WHERE c.oid = 'public.context_lifecycles'::regclass
`).Scan(&indexExists, &rlsEnabled, &rlsForced, &statusValues, &sessionRunFKs, &tenantFKs, &tenantKeyFound); err != nil {
		t.Fatalf("inspect context lifecycle schema: %v", err)
	}
	if !indexExists || !rlsEnabled || !rlsForced || sessionRunFKs != 0 || tenantFKs != 2 || !tenantKeyFound {
		t.Fatalf(
			"context lifecycle schema = index:%t rls:%t force:%t session_run_fks:%d tenant_fks:%d tenant_key:%t",
			indexExists, rlsEnabled, rlsForced, sessionRunFKs, tenantFKs, tenantKeyFound,
		)
	}
	for _, status := range []string{"completed", "failed_budget", "failed_provider", "fallback", "aborted"} {
		if !strings.Contains(statusValues, status) {
			t.Fatalf("context lifecycle status CHECK %q is missing %q", statusValues, status)
		}
	}
}

func mustParseUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	parsed, err := dbpkg.ParseUUID(value)
	if err != nil {
		t.Fatalf("parse UUID %q: %v", value, err)
	}
	return parsed
}
