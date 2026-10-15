package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The import moves what servers of the previous release left in Redis into the
// queue table. It needs both, so each case is skipped unless TEST_POSTGRES_DSN
// and MEMOH_TEST_REDIS_URL name them.

func newLegacyImportEnv(t *testing.T) (queueEnv, *RedisBackend, *Manager) {
	t.Helper()
	redisURL := os.Getenv("MEMOH_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set MEMOH_TEST_REDIS_URL to run the legacy follow-up import tests")
	}
	env := newQueueEnv(t)
	backend, err := NewRedisBackend(context.Background(), RedisOptions{
		URL: redisURL, KeyPrefix: uniqueRuntimeBackendPrefix("legacy-import"), StateTTL: time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	manager := NewManager(backend, Options{})
	t.Cleanup(func() { _ = manager.Close() })
	return env, backend, manager
}

// legacyDocument writes a follow-up queue the way the previous release did.
func legacyDocument(t *testing.T, backend *RedisBackend, key Key, items ...legacyFollowUpItem) {
	t.Helper()
	state := legacyFollowUpState{Items: items, UpdatedAt: time.Now()}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, backend.client.Set(context.Background(), backend.legacyFollowUpKey(key), raw, time.Minute).Err())
}

func legacyItem(key Key, id, text string, position int64) legacyFollowUpItem {
	return legacyFollowUpItem{
		ID: id, BotID: key.BotID, SessionID: key.SessionID, InvocationID: "invocation-" + id,
		Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)), Status: QueueAccepted, Position: position,
		CreatedAt: time.Now().Add(-time.Duration(10-position) * time.Minute).UTC(),
	}
}

func asEntry(item LegacyFollowUp) (QueueEnqueue, bool, error) {
	return QueueEnqueue{
		Key: item.Key, Origin: QueueOriginFollowUp, InvocationID: item.InvocationID,
		Payload: item.Payload, EnqueuedDuringRunID: item.EnqueuedDuringRunID, CreatedAt: item.CreatedAt,
	}, true, nil
}

func (e queueEnv) redisEmpty(t *testing.T, backend *RedisBackend) bool {
	t.Helper()
	keys, err := backend.LegacyFollowUpKeys(context.Background())
	require.NoError(t, err)
	for _, key := range keys {
		if key == e.key {
			return false
		}
	}
	return true
}

func TestImportLegacyFollowUpsMovesTheQueueAndEmptiesRedis(t *testing.T) {
	env, backend, manager := newLegacyImportEnv(t)
	ctx := context.Background()
	legacyDocument(t, backend, env.key,
		legacyItem(env.key, "a", "first", 1), legacyItem(env.key, "b", "second", 2), legacyItem(env.key, "c", "third", 3))

	moved, err := manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Equal(t, 3, moved)
	require.True(t, env.redisEmpty(t, backend), "nothing is left to start in Redis")

	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second", "third"}, texts(pending), "the queue keeps its order")
	require.Equal(t, "invocation-a", pending[0].InvocationID)

	moved, err = manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Zero(t, moved, "a second pass finds nothing")
}

// An old server keeps writing to Redis until it is gone, so what it writes
// after a pass is taken by the next one.
func TestImportLegacyFollowUpsKeepsTakingWhatOldServersWrite(t *testing.T) {
	env, backend, manager := newLegacyImportEnv(t)
	ctx := context.Background()
	legacyDocument(t, backend, env.key, legacyItem(env.key, "a", "early", 1))
	moved, err := manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Equal(t, 1, moved)

	legacyDocument(t, backend, env.key, legacyItem(env.key, "z", "late", 1))
	moved, err = manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Equal(t, 1, moved)
	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Equal(t, []string{"early", "late"}, texts(pending))
}

// A process killed between writing the input to the table and removing it from
// Redis leaves it in both. The next pass finds the claim it left, writes the
// input under the invocation id it already has, and removes it.
func TestImportLegacyFollowUpsSurvivesAKillBetweenWriteAndRemoval(t *testing.T) {
	env, backend, manager := newLegacyImportEnv(t)
	ctx := context.Background()
	legacyDocument(t, backend, env.key, legacyItem(env.key, "a", "half moved", 1), legacyItem(env.key, "b", "not yet", 2))

	claimed, ok, err := backend.ClaimLegacyFollowUp(ctx, env.key)
	require.NoError(t, err)
	require.True(t, ok)
	entry, _, _ := asEntry(claimed)
	_, err = env.store.Enqueue(ctx, entry)
	require.NoError(t, err)
	// The process dies here: the claim stands, the removal never happens.

	moved, err := manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Equal(t, 2, moved)
	require.True(t, env.redisEmpty(t, backend))
	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Equal(t, []string{"half moved", "not yet"}, texts(pending), "the half-moved input is in the table once")
}

// A server of the previous release that claimed an item is starting it. The
// import does not take an item out of its hands.
func TestImportLegacyFollowUpsLeavesItemsAnOldServerHolds(t *testing.T) {
	env, backend, manager := newLegacyImportEnv(t)
	ctx := context.Background()
	held := legacyItem(env.key, "held", "being started", 1)
	held.Status = QueueClaimed
	held.Claim = &legacyFollowUpClaim{ItemID: "held", TriggerRunID: "run-that-ended", ClaimToken: "token"}
	legacyDocument(t, backend, env.key, held, legacyItem(env.key, "free", "waiting", 2))

	moved, err := manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Equal(t, 1, moved)
	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Equal(t, []string{"waiting"}, texts(pending))
	require.False(t, env.redisEmpty(t, backend), "the held item stays with the old server")
}

func TestImportLegacyFollowUpsLeavesWhatThePassDoesNotOwnAndDropsWhatCannotBeRead(t *testing.T) {
	env, backend, manager := newLegacyImportEnv(t)
	ctx := context.Background()
	legacyDocument(t, backend, env.key, legacyItem(env.key, "other-team", "elsewhere", 1))
	moved, err := manager.ImportLegacyFollowUps(ctx, env.store, func(LegacyFollowUp) (QueueEnqueue, bool, error) {
		return QueueEnqueue{}, false, nil
	})
	require.NoError(t, err)
	require.Zero(t, moved)
	claimed, ok, err := backend.ClaimLegacyFollowUp(ctx, env.key)
	require.NoError(t, err)
	require.True(t, ok, "the item went back as it was, for the pass of its own scope")
	require.Equal(t, "other-team", claimed.ID)
	require.NoError(t, backend.ReleaseLegacyFollowUp(ctx, env.key, claimed.ID))

	moved, err = manager.ImportLegacyFollowUps(ctx, env.store, func(LegacyFollowUp) (QueueEnqueue, bool, error) {
		return QueueEnqueue{}, false, errors.New("unreadable")
	})
	require.NoError(t, err)
	require.Equal(t, 1, moved, "an input nobody can start is dropped, not retried at every pass")
	require.True(t, env.redisEmpty(t, backend))
	pending, err := env.store.Pending(ctx, env.key, QueueOriginFollowUp)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestImportLegacyFollowUpsStopsAtAFullQueue(t *testing.T) {
	env, backend, manager := newLegacyImportEnv(t)
	ctx := context.Background()
	for i := range MaxPendingQueueItems {
		env.enqueue(t, QueueOriginFollowUp, fmt.Sprintf("fill-%d", i), "", "x")
	}
	legacyDocument(t, backend, env.key, legacyItem(env.key, "a", "waits in redis", 1))
	moved, err := manager.ImportLegacyFollowUps(ctx, env.store, asEntry)
	require.NoError(t, err)
	require.Zero(t, moved)
	require.False(t, env.redisEmpty(t, backend), "the input stays in Redis until there is room")
	_, ok, err := backend.ClaimLegacyFollowUp(ctx, env.key)
	require.NoError(t, err)
	require.True(t, ok, "and it was released, not left claimed")
}

// The memory backend never held follow-ups across a restart, so the step has
// nothing to do for it.
func TestImportLegacyFollowUpsIsNothingWithTheMemoryBackend(t *testing.T) {
	manager := NewManager(NewMemoryBackend(), Options{})
	t.Cleanup(func() { _ = manager.Close() })
	moved, err := manager.ImportLegacyFollowUps(context.Background(), nil, func(LegacyFollowUp) (QueueEnqueue, bool, error) {
		t.Error("the memory backend has no legacy follow-ups to convert")
		return QueueEnqueue{}, false, nil
	})
	require.NoError(t, err)
	require.Zero(t, moved)
}
