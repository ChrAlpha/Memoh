package thread

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbpkg "github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/dbtest"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

// Independent connections compete for the last slot, then a fresh service
// reconciles the hold after history deletion and soft deletion of the child.
func TestPostgresExternalChildCapacitySurvivesDeletionAndRestart(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN")
	}
	if os.Getenv("TEST_POSTGRES_BOOTSTRAP_SCHEMA") == "1" {
		if err := dbtest.MigratePostgresUp(dsn); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	pool, err := dbpkg.OpenPostgresDSN(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, bot, parent := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`WITH u AS (INSERT INTO users (id,username) VALUES ($1::uuid,$1::text) RETURNING id) INSERT INTO team_members (user_id,role) SELECT id,'admin' FROM u`, owner)
	exec(`INSERT INTO bots (id,owner_user_id,name) VALUES ($1,$2,'child-capacity-test')`, bot, owner)
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM bots WHERE id=$1`, bot)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, owner)
	}()
	exec(`INSERT INTO bot_sessions (id,bot_id) VALUES ($1,$2)`, parent, bot)
	freshService := func() *Service { return NewService(nil, postgresstore.NewQueriesWithPool(pool, sqlc.New(pool)), nil) }
	const workers = 12
	children := make([]Thread, workers)
	runs := make([]string, workers)
	for i := range children {
		children[i] = Thread{ID: uuid.NewString(), BotID: bot, RuntimeType: RuntimeCodex, SessionMode: TypeSubagent}
		runs[i] = uuid.NewString()
		exec(`INSERT INTO bot_sessions (id,bot_id,parent_session_id,type,session_mode,runtime_type,runtime_fencing_token) VALUES ($1,$2,$3,'subagent','subagent','codex',7)`, children[i].ID, bot, parent)
	}
	fence := func(child Thread) context.Context {
		return runtimefence.WithContext(ctx, runtimefence.Fence{BotID: bot, SessionID: child.ID, Token: 7})
	}
	start := make(chan struct{})
	results := make(chan int, workers)
	var wg sync.WaitGroup
	for i := range children {
		wg.Go(func() {
			<-start
			err := freshService().BeginExternalSubagentExecution(fence(children[i]), children[i], runs[i], 1)
			if err == nil {
				results <- i
			} else if !errors.Is(err, ErrExternalChildCapacity) {
				t.Errorf("reserve: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := []int{}
	for i := range results {
		winners = append(winners, i)
	}
	if len(winners) != 1 {
		t.Fatalf("reserved %d slots, want one", len(winners))
	}
	i := winners[0]
	child, run := children[i], runs[i]
	q := postgresstore.NewQueriesWithPool(pool, sqlc.New(pool))
	s := freshService()
	// Public metadata replacement cannot erase the application-owned hold.
	if _, err := s.UpdateDescriptorAndMetadataWithOwner(ctx, child.ID, TypeSubagent, TypeSubagent, RuntimeCodex, map[string]any{}, map[string]any{}, nil, owner); !errors.Is(err, ErrSubagentBindingImmutable) {
		t.Fatalf("metadata replacement: %v", err)
	}
	exec(`INSERT INTO session_runs (run_id,bot_id,session_id,invocation_id,turn_id,turn_position,state,input_json,input_fingerprint) VALUES ($1,$2,$3,'child-test',$4,1,'running','{}','test')`, run, bot, child.ID, uuid.NewString())
	claim := uuid.NewString()
	if ok, err := s.ClaimExternalSubagentReap(ctx, child.ID, run, claim); err != nil || ok {
		t.Fatalf("claimed live run: %v, %v", ok, err)
	}
	// Both history reset routes preserve the execution receipt, while retiring
	// the old session run under the ordinary history-reset fence.
	if err := q.ClearHistoryBySession(ctx, dbpkg.ParseUUIDOrEmpty(child.ID)); err != nil {
		t.Fatal(err)
	}
	if err := q.ClearHistoryByBot(ctx, dbpkg.ParseUUIDOrEmpty(bot)); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE bot_sessions SET deleted_at=now() WHERE id=$1`, child.ID)
	holds, err := s.ListExternalExecutionHolds(ctx, bot)
	if err != nil || len(holds) != 1 {
		t.Fatalf("restart holds: %+v, %v", holds, err)
	}
	listed, err := q.ListSubagentSessionsByParent(ctx, dbpkg.ParseUUIDOrEmpty(parent))
	found := false
	for _, row := range listed {
		if row.ID.String() == child.ID {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("deleted hold disappeared from discovery: %v", err)
	}
	if err := s.FinishExternalSubagentExecution(fence(child), child.ID, run); !errors.Is(err, ErrRuntimeMetadataStale) {
		t.Fatalf("pre-reset fence released hold: %v", err)
	}
	if ok, err := s.ClaimExternalSubagentReap(ctx, child.ID, uuid.NewString(), claim); err != nil || ok {
		t.Fatalf("wrong run claimed: %v, %v", ok, err)
	}
	exec(`UPDATE session_runs SET state='lost' WHERE run_id=$1`, run)
	if ok, err := s.ClaimExternalSubagentReap(ctx, child.ID, run, claim); err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}
	if ok, err := s.ResolveExternalSubagentTermination(ctx, child.ID, run, uuid.NewString()); err != nil || ok {
		t.Fatalf("wrong claim released hold: %v, %v", ok, err)
	}
	if ok, err := s.ResolveExternalSubagentTermination(ctx, child.ID, run, claim); err != nil || !ok {
		t.Fatalf("resolve: %v, %v", ok, err)
	}
	holds, err = s.ListExternalExecutionHolds(ctx, bot)
	if err != nil || len(holds) != 0 {
		t.Fatalf("resolved holds: %+v, %v", holds, err)
	}
	// Even restoring the thread cannot replay an already dispatched one-shot.
	exec(`UPDATE bot_sessions SET deleted_at=NULL,runtime_fencing_token=7 WHERE id=$1`, child.ID)
	if err := s.BeginExternalSubagentExecution(fence(child), child, uuid.NewString(), 1); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replay admitted: %v", err)
	}
}
