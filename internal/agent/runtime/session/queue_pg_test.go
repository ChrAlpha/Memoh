package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	dbpkg "github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// These tests run the session input queue against PostgreSQL, where the claim,
// the lease and the order are decided. Each case is skipped unless
// TEST_POSTGRES_DSN names a migrated database.

type queueEnv struct {
	pool  *pgxpool.Pool
	store *QueuedInputs
	key   Key
}

func openQueuePostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the session input queue PostgreSQL tests")
	}
	pool, err := dbpkg.OpenPostgresDSN(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// newQueueEnv is a bot with one session, and a queue over them.
func newQueueEnv(t *testing.T, opts ...QueuedInputsOption) queueEnv {
	t.Helper()
	ctx := context.Background()
	pool := openQueuePostgres(t)
	userID, botID, sessionID := uuid.New(), uuid.New(), uuid.New()
	name := "session-queue-" + uuid.NewString()
	_, err := pool.Exec(ctx, `
		WITH created_user AS (
			INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
		)
		INSERT INTO team_members (user_id, role) SELECT id, 'admin' FROM created_user
	`, userID, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, botID, userID, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')`, sessionID, botID)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_, _ = pool.Exec(cleanup, "DELETE FROM bots WHERE id = $1", botID)
		_, _ = pool.Exec(cleanup, "DELETE FROM users WHERE id = $1", userID)
	})
	return queueEnv{
		pool:  pool,
		store: NewQueuedInputs(dbsqlc.New(pool), pool, opts...),
		key:   Key{BotID: botID.String(), SessionID: sessionID.String()},
	}
}

func (e queueEnv) enqueue(t *testing.T, origin QueueOrigin, invocation, mergeKey, text string) QueuedInput {
	t.Helper()
	item, err := e.store.Enqueue(context.Background(), QueueEnqueue{
		Key: e.key, Origin: origin, InvocationID: invocation, MergeKey: mergeKey,
		Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)),
	})
	require.NoError(t, err)
	return item
}

// holdRun puts a run of the session in flight, and returns what ends it.
func (e queueEnv) holdRun(t *testing.T) (end func()) {
	t.Helper()
	runID := uuid.New()
	_, err := e.pool.Exec(context.Background(), `
		INSERT INTO session_runs (run_id, bot_id, session_id, invocation_id, turn_id, turn_position, state, input_json, input_fingerprint)
		VALUES ($1, $2, $3, $4, $5, 1, 'running', '{}', 'x')
	`, runID, e.key.BotID, e.key.SessionID, "held-"+runID.String(), uuid.New())
	require.NoError(t, err)
	return func() {
		_, err := e.pool.Exec(context.Background(), `UPDATE session_runs SET state = 'completed' WHERE run_id = $1`, runID)
		require.NoError(t, err)
	}
}

func (e queueEnv) state(t *testing.T, id string) string {
	t.Helper()
	var state string
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT state FROM session_queued_inputs WHERE id = $1`, id).Scan(&state))
	return state
}

func texts(inputs []QueuedInput) []string {
	out := make([]string, len(inputs))
	for i, input := range inputs {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(input.Payload, &body)
		out[i] = body.Text
	}
	return out
}

func TestQueuedInputsEnqueueRepeatsAndConflicts(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	first := env.enqueue(t, QueueOriginFollowUp, "msg-1", "", "hello")

	again, err := env.store.Enqueue(ctx, QueueEnqueue{
		Key: env.key, Origin: QueueOriginFollowUp, InvocationID: "msg-1",
		Payload: []byte(`{ "text" : "hello" }`),
	})
	require.NoError(t, err, "the same message with its keys in another order is one input")
	require.Equal(t, first.ID, again.ID)

	_, err = env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: QueueOriginFollowUp, InvocationID: "msg-1", Payload: []byte(`{"text":"changed"}`)})
	require.ErrorIs(t, err, ErrQueueInvocationConflict)

	_, err = env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: QueueOriginPlatformBusy, InvocationID: "msg-1", Payload: []byte(`{"text":"hello"}`)})
	require.NoError(t, err, "the identity is the session's, whichever origin repeats it")

	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	_, err = env.store.Enqueue(ctx, QueueEnqueue{Key: Key{BotID: env.key.BotID, SessionID: uuid.NewString()}, Origin: QueueOriginFollowUp, InvocationID: "x", Payload: []byte(`{}`)})
	require.ErrorIs(t, err, ErrQueueInvalidReference, "a session that does not exist takes no input")
	_, err = env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: "elsewhere", InvocationID: "y", Payload: []byte(`{}`)})
	require.ErrorIs(t, err, ErrQueueInvalidReference)
}

func TestQueuedInputsRefuseBeyondTheBoundAndSaySo(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	for i := range MaxPendingQueueItems {
		env.enqueue(t, QueueOriginPlatformBusy, fmt.Sprintf("m%d", i), "", "x")
	}
	_, err := env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: QueueOriginPlatformBusy, InvocationID: "overflow", Payload: []byte(`{}`)})
	require.ErrorIs(t, err, ErrQueueCapacityExceeded, "the sender is told, the input is not dropped quietly")

	again, err := env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: QueueOriginPlatformBusy, InvocationID: "m0", Payload: []byte(`{"text":"x"}`)})
	require.NoError(t, err, "a message the queue already holds is still found when it is full")
	require.Equal(t, "m0", again.InvocationID)

	_, err = env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: QueueOriginFollowUp, InvocationID: "follow", Payload: []byte(`{}`)})
	require.NoError(t, err, "each origin has its own bound")
}

func TestQueuedInputsBoundHoldsUnderConcurrentWriters(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := range 2 * MaxPendingQueueItems {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.store.Enqueue(ctx, QueueEnqueue{Key: env.key, Origin: QueueOriginPlatformBusy, InvocationID: fmt.Sprintf("c%d", i), Payload: []byte(`{}`)})
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else if !errors.Is(err, ErrQueueCapacityExceeded) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, MaxPendingQueueItems, accepted)
}

func TestQueuedInputsReorderTradesPositionsAndRefusesWhatLeftTheQueue(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	var ids []string
	for _, name := range []string{"a", "b", "c", "d"} {
		ids = append(ids, env.enqueue(t, QueueOriginFollowUp, name, "", name).ID)
	}
	before, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)

	_, err = env.store.Reorder(ctx, env.key, QueueOriginFollowUp, ids[3], ids[0])
	require.NoError(t, err)
	got, err := env.store.Reorder(ctx, env.key, QueueOriginFollowUp, ids[2], ids[1])
	require.NoError(t, err)
	require.Equal(t, []string{ids[3], ids[0], ids[2], ids[1]}, []string{got[0].ID, got[1].ID, got[2].ID, got[3].ID},
		"the second move starts from the order the first left")
	for i := range got {
		require.Equal(t, before[i].Position, got[i].Position, "the positions are the same values in another order")
	}

	last, err := env.store.Reorder(ctx, env.key, QueueOriginFollowUp, ids[3], "")
	require.NoError(t, err)
	require.Equal(t, ids[3], last[3].ID, "an empty before moves to the end")

	require.NoError(t, env.store.Cancel(ctx, env.key, QueueOriginFollowUp, ids[1]))
	_, err = env.store.Reorder(ctx, env.key, QueueOriginFollowUp, ids[1], ids[0])
	require.ErrorIs(t, err, ErrQueueNotPending)
	require.ErrorIs(t, env.store.Cancel(ctx, env.key, QueueOriginFollowUp, ids[1]), ErrQueueNotPending)
	_, err = env.store.Edit(ctx, env.key, QueueOriginFollowUp, ids[1], []byte(`{"text":"late"}`))
	require.ErrorIs(t, err, ErrQueueNotPending)
	edited, err := env.store.Edit(ctx, env.key, QueueOriginFollowUp, ids[0], []byte(`{"text":"edited"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"text":"edited"}`, string(edited.Payload))
}

func TestQueuedInputsBatchGroupsNeighboursWithOneMergeKey(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	for _, in := range [][3]string{
		{"1", "alice", "a1"},
		{"2", "alice", "a2"},
		{"3", "bob", "b1"},
		{"4", "alice", "a3"},
		{"5", "", "alone-1"},
		{"6", "", "alone-2"},
	} {
		env.enqueue(t, QueueOriginPlatformBusy, in[0], in[1], in[2])
	}
	var batches [][]string
	for {
		batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
		require.NoError(t, err)
		if !ok {
			break
		}
		batches = append(batches, texts(batch.Items))
		require.NoError(t, env.store.SettleBatch(ctx, batch))
	}
	require.Equal(t, [][]string{
		{"a1", "a2"}, {"b1"}, {"a3"}, {"alone-1"}, {"alone-2"},
	}, batches, "a batch is the neighbours of one sender, and an input without a key stands alone")
}

func TestQueuedInputsClaimWaitsForAFreeSession(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	env.enqueue(t, QueueOriginPlatformBusy, "1", "alice", "waiting")
	end := env.holdRun(t)

	_, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.False(t, ok, "nothing is claimed while a run holds the session")

	end()
	batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, batch.Items, 1)
	require.False(t, batch.Recovered)

	_, ok, err = env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.False(t, ok, "a batch being admitted is not claimed twice")
}

func TestQueuedInputsClaimedByOneOfTwoDrainers(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	env.enqueue(t, QueueOriginFollowUp, "1", "", "only one run")

	const drainers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []QueueBatch
	for range drainers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginFollowUp)
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				winners = append(winners, batch)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Len(t, winners, 1, "one drainer takes the batch")
}

// SKIP LOCKED is what keeps a drainer that finds the rows locked from waiting
// for the one that holds them: it has nothing to claim and says so at once.
func TestQueuedInputsClaimSkipsRowsAnotherDrainerHolds(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	env.enqueue(t, QueueOriginFollowUp, "1", "", "held")

	tx, err := env.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := dbsqlc.New(env.pool).WithTx(tx).ListQueuedInputsToClaim(ctx, dbsqlc.ListQueuedInputsToClaimParams{
		SessionID: dbpkg.ParseUUIDOrEmpty(env.key.SessionID), Origin: string(QueueOriginFollowUp),
	})
	require.NoError(t, err)
	require.Len(t, rows, 1, "this drainer holds the row")

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginFollowUp)
		done <- result{ok, err}
	}()
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.False(t, got.ok, "the other drainer found nothing to take")
	case <-time.After(3 * time.Second):
		t.Fatal("a drainer waited for rows another drainer holds")
	}

	require.NoError(t, tx.Rollback(ctx))
	_, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.True(t, ok, "released rows are claimed")
}

func TestQueuedInputsLapsedBatchIsClaimedAgainUnderItsOwnID(t *testing.T) {
	env := newQueueEnv(t, WithQueueClaimLease(150*time.Millisecond))
	ctx := context.Background()
	first := env.enqueue(t, QueueOriginPlatformBusy, "1", "alice", "first")
	batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.True(t, ok)

	// A newer input arrives while the batch is being admitted.
	later := env.enqueue(t, QueueOriginPlatformBusy, "2", "alice", "later")
	_, ok, err = env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.False(t, ok, "a live claim holds the queue behind it")

	time.Sleep(300 * time.Millisecond)
	recovered, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.True(t, ok, "the lease ran out")
	require.True(t, recovered.Recovered)
	require.Equal(t, batch.ID, recovered.ID, "the same batch, so the same invocation id")
	require.Equal(t, batch.InvocationID(), recovered.InvocationID())
	require.Len(t, recovered.Items, 1)
	require.Equal(t, first.ID, recovered.Items[0].ID, "the newer input is not folded into it")
	require.NotEqual(t, batch.ClaimToken, recovered.ClaimToken)

	require.NoError(t, env.store.ReleaseBatch(ctx, recovered))
	require.Equal(t, string(QueuedStateClaimed), env.state(t, first.ID),
		"a recovered batch is not handed back: an earlier attempt may have admitted its run")

	require.NoError(t, env.store.SettleBatch(ctx, recovered))
	require.Equal(t, string(QueuedStateConsumed), env.state(t, first.ID))
	next, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, later.ID, next.Items[0].ID)
}

func TestQueuedInputsReleaseKeepsThePlaceInLine(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	first := env.enqueue(t, QueueOriginFollowUp, "1", "", "first")
	env.enqueue(t, QueueOriginFollowUp, "2", "", "second")
	batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.True(t, ok)

	other := batch
	other.ClaimToken = uuid.NewString()
	require.NoError(t, env.store.ReleaseBatch(ctx, other))
	require.Equal(t, string(QueuedStateClaimed), env.state(t, first.ID), "only the holder of the claim releases it")

	require.NoError(t, env.store.ReleaseBatch(ctx, batch))
	again, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, first.ID, again.Items[0].ID)
	require.NotEqual(t, batch.ID, again.ID, "a released batch is a new batch")
}

func TestQueuedInputsGateKnowsWhatIsQueuedAndWhatWasTaken(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	gate, err := env.store.Gate(ctx, env.key, "msg-1")
	require.NoError(t, err)
	require.Equal(t, QueueGate{}, gate)

	env.enqueue(t, QueueOriginPlatformBusy, "msg-1", "alice", "hi")
	gate, err = env.store.Gate(ctx, env.key, "msg-1")
	require.NoError(t, err)
	require.Equal(t, QueueGate{Seen: true, Waiting: true}, gate)
	gate, err = env.store.Gate(ctx, env.key, "msg-2")
	require.NoError(t, err)
	require.Equal(t, QueueGate{Waiting: true}, gate, "another message waits behind it")
	gate, err = env.store.Gate(ctx, env.key, "")
	require.NoError(t, err)
	require.False(t, gate.Seen, "a turn without an identity has not been seen")

	batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.True(t, ok)
	gate, err = env.store.Gate(ctx, env.key, "msg-2")
	require.NoError(t, err)
	require.True(t, gate.Waiting, "a batch being admitted still holds the queue")
	require.NoError(t, env.store.SettleBatch(ctx, batch))
	gate, err = env.store.Gate(ctx, env.key, "msg-1")
	require.NoError(t, err)
	require.Equal(t, QueueGate{Seen: true}, gate, "a taken message is still recognized when it is delivered again")
}

func TestQueuedInputsRejectedInputLeavesTheQueue(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	broken := env.enqueue(t, QueueOriginFollowUp, "1", "", "broken")
	next := env.enqueue(t, QueueOriginFollowUp, "2", "", "next")
	require.ErrorIs(t, env.store.Reject(ctx, broken.ID, ""), ErrQueueInvalidReference)
	require.NoError(t, env.store.Reject(ctx, broken.ID, QueueErrorFollowUpCommandInvalid))
	var code string
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT error_code FROM session_queued_inputs WHERE id = $1`, broken.ID).Scan(&code))
	require.Equal(t, QueueErrorFollowUpCommandInvalid, code)
	batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, next.ID, batch.Items[0].ID)
}

func TestQueuedInputsDrainableSessionsAndPurge(t *testing.T) {
	env := newQueueEnv(t, WithQueueClaimLease(100*time.Millisecond))
	ctx := context.Background()
	drainable := func(origin QueueOrigin) []Key {
		keys, err := env.store.Drainable(ctx, origin)
		require.NoError(t, err)
		var mine []Key
		for _, key := range keys {
			if key == env.key {
				mine = append(mine, key)
			}
		}
		return mine
	}
	require.Empty(t, drainable(QueueOriginPlatformBusy))
	env.enqueue(t, QueueOriginPlatformBusy, "1", "alice", "hi")
	require.Len(t, drainable(QueueOriginPlatformBusy), 1)
	require.Empty(t, drainable(QueueOriginFollowUp), "each origin is drained by its own process")

	batch, ok, err := env.store.ClaimBatch(ctx, env.key, QueueOriginPlatformBusy)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, drainable(QueueOriginPlatformBusy), "a live claim has a drainer already")
	time.Sleep(250 * time.Millisecond)
	require.Len(t, drainable(QueueOriginPlatformBusy), 1, "a lapsed claim is drainable again")

	require.NoError(t, env.store.SettleBatch(ctx, batch))
	require.Empty(t, drainable(QueueOriginPlatformBusy))
	purged, err := env.store.PurgeSettled(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Zero(t, purged, "recent input is kept")
	purged, err = env.store.PurgeSettled(ctx, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.GreaterOrEqual(t, purged, int64(1))
	gate, err := env.store.Gate(ctx, env.key, "1")
	require.NoError(t, err)
	require.False(t, gate.Seen)
}

func TestQueuedInputsPromotionSteps(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	item := env.enqueue(t, QueueOriginFollowUp, "1", "", "promote me")
	platform := env.enqueue(t, QueueOriginPlatformBusy, "2", "alice", "not a follow-up")

	_, err := env.store.BeginPromotion(ctx, env.key, platform.ID)
	require.ErrorIs(t, err, ErrQueueNotPending, "only a follow-up is promoted")

	promotion, err := env.store.BeginPromotion(ctx, env.key, item.ID)
	require.NoError(t, err)
	require.False(t, promotion.Replay)
	require.NotEmpty(t, promotion.Token)
	require.Equal(t, string(QueuedStatePromoting), env.state(t, item.ID))

	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Empty(t, pending, "a promoting input is not offered to a drainer")
	repeat, err := env.store.BeginPromotion(ctx, env.key, item.ID)
	require.NoError(t, err)
	require.True(t, repeat.Replay)

	require.NoError(t, env.store.AbortPromotion(ctx, item.ID, promotion.Token))
	require.Equal(t, string(QueuedStateQueued), env.state(t, item.ID), "an aborted promotion returns the input")

	promotion, err = env.store.BeginPromotion(ctx, env.key, item.ID)
	require.NoError(t, err)
	require.NoError(t, env.store.CompletePromotion(ctx, item.ID, "00000000-0000-0000-0000-000000000000"))
	require.Equal(t, string(QueuedStatePromoting), env.state(t, item.ID), "a stale token completes nothing")
	require.NoError(t, env.store.CompletePromotion(ctx, item.ID, promotion.Token))
	require.Equal(t, string(QueuedStatePromoted), env.state(t, item.ID))

	listed, err := env.store.Promotions(ctx, env.key)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	stale, err := env.store.StalePromotions(ctx, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NotEmpty(t, stale)
	fresh, err := env.store.StalePromotions(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	for _, input := range fresh {
		require.NotEqual(t, item.ID, input.ID, "a promotion that is not old is not stale")
	}

	require.NoError(t, env.store.RequeuePromoted(ctx, item.ID))
	require.Equal(t, string(QueuedStateQueued), env.state(t, item.ID))
	_, err = env.store.BeginPromotion(ctx, env.key, item.ID)
	require.NoError(t, err)
	require.NoError(t, env.store.ConsumePromoted(ctx, item.ID))
	require.Equal(t, string(QueuedStateConsumed), env.state(t, item.ID))
	require.NoError(t, env.store.RequeuePromoted(ctx, item.ID))
	require.Equal(t, string(QueuedStateConsumed), env.state(t, item.ID), "a consumed input is never put back")
}

func TestQueuedInputsDieWithTheirSession(t *testing.T) {
	env := newQueueEnv(t)
	ctx := context.Background()
	item := env.enqueue(t, QueueOriginFollowUp, "1", "", "gone")
	_, err := env.pool.Exec(ctx, `DELETE FROM bot_sessions WHERE id = $1`, env.key.SessionID)
	require.NoError(t, err)
	var id string
	err = env.pool.QueryRow(ctx, `SELECT id::text FROM session_queued_inputs WHERE id = $1`, item.ID).Scan(&id)
	require.ErrorIs(t, err, pgx.ErrNoRows)
}
