package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisQueueMaxRetries = 8

func (b *RedisBackend) ensureQueueOpen() error {
	if b == nil || b.client == nil {
		return ErrLiveQueueUnavailable
	}
	b.subscriptionsMu.Lock()
	closed := b.closed
	b.subscriptionsMu.Unlock()
	if closed {
		return ErrLiveQueueUnavailable
	}
	return nil
}

func waitRedisQueueRetry(ctx context.Context, attempt int) error {
	if attempt >= redisQueueMaxRetries {
		return ErrQueueAdmissionOverloaded
	}
	delay := time.Duration(1<<min(attempt, 6)) * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func redisWatch(ctx context.Context, b *RedisBackend, keys []string, fn func(*redis.Tx) error) error {
	if err := b.ensureQueueOpen(); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := b.client.Watch(ctx, fn, keys...)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
		if err := waitRedisQueueRetry(ctx, attempt); err != nil {
			return err
		}
		if err := b.ensureQueueOpen(); err != nil {
			return err
		}
	}
}

func loadRedisJSON[T any](ctx context.Context, cmd redis.Cmdable, key string) (T, error) {
	var value T
	data, err := cmd.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return value, nil
	}
	if err != nil {
		return value, err
	}
	return value, json.Unmarshal(data, &value)
}

func storeRedisJSON(ctx context.Context, tx *redis.Tx, key string, value any, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, key, data, ttl)
		return nil
	})
	return err
}

// compactableQueueState is implemented by both queue documents so the generic
// mutation helpers can bound terminal items before writing the document back.
type compactableQueueState interface{ compact() }

func redisMutate[T any, R any](ctx context.Context, b *RedisBackend, key string, mutate func(*T, time.Time) (R, error)) (R, error) {
	return redisMutateWithKeys(ctx, b, []string{key}, key, func(_ *redis.Tx, state *T, now time.Time) (R, error) {
		return mutate(state, now)
	})
}

// redisMutateWithKeys runs one optimistic transaction over a queue document.
// keys is the WATCH set; the document itself must be part of it. Callers may
// read other keys inside mutate without watching them: the queue document is
// the serialization point for queue decisions, and run ownership is watched
// through the run key where a decision depends on it. The session state key is
// deliberately never watched here because it is rewritten on every streamed
// runtime delta and would make queue transactions fail under normal output.
func redisMutateWithKeys[T any, R any](ctx context.Context, b *RedisBackend, keys []string, key string, mutate func(*redis.Tx, *T, time.Time) (R, error)) (R, error) {
	var zero R
	if err := b.ensureQueueOpen(); err != nil {
		return zero, err
	}
	var result R
	err := redisWatch(ctx, b, keys, func(tx *redis.Tx) error {
		state, err := loadRedisJSON[T](ctx, tx, key)
		if err != nil {
			return err
		}
		now, err := tx.Time(ctx).Result()
		if err != nil {
			return err
		}
		result, err = mutate(tx, &state, now.UTC())
		if err != nil {
			return err
		}
		if compactable, ok := any(&state).(compactableQueueState); ok {
			compactable.compact()
		}
		return storeRedisJSON(ctx, tx, key, state, b.stateTTL)
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}

func (b *RedisBackend) EnqueueSteer(ctx context.Context, key Key, itemID, invocationID string, payload []byte) (SteerItem, error) {
	if err := b.ensureQueueOpen(); err != nil {
		return SteerItem{}, err
	}
	if err := validateQueueKey(key); err != nil || strings.TrimSpace(itemID) == "" || strings.TrimSpace(invocationID) == "" || validatePayload(payload) != nil {
		return SteerItem{}, ErrQueueInvalidReference
	}
	stateKey, queueKey := b.stateKey(key), b.steerQueueKey(key)
	var item SteerItem
	// The state key is read but not watched: a run that terminalizes between
	// this read and EXEC is closed by CloseSteerRun, which serializes on the
	// queue key and rejects the item or has already sealed the run ID.
	err := redisWatch(ctx, b, []string{queueKey}, func(tx *redis.Tx) error {
		queue, err := loadRedisJSON[steerQueueState](ctx, tx, queueKey)
		if err != nil {
			return err
		}
		if replay, ok, replayErr := replaySteer(queue, invocationID, payload); ok {
			item = replay
			return replayErr
		}
		snapshot, ok, err := loadRedisSnapshot(ctx, tx, stateKey)
		if err != nil {
			return err
		}
		run, active := activeRun(snapshot, ok)
		if !active || queue.ClosedRunID == run.RunID {
			return ErrQueueNoActiveRun
		}
		if !SteerRunAvailable(run) {
			return ErrQueueSteerUnsupported
		}
		now, err := tx.Time(ctx).Result()
		if err != nil {
			return err
		}
		item, err = queue.enqueue(key, itemID, invocationID, run.RunID, payload, now.UTC())
		if err != nil {
			return err
		}
		return storeRedisJSON(ctx, tx, queueKey, queue, b.stateTTL)
	})
	return item, err
}

func (b *RedisBackend) PendingSteers(ctx context.Context, key Key, limit int) ([]SteerItem, error) {
	steers, err := b.loadSteerQueue(ctx, key)
	if err != nil {
		return nil, err
	}
	return pendingSteers(steers, limit), nil
}

func (b *RedisBackend) LookupSteer(ctx context.Context, key Key, itemID SteerItemID) (SteerItem, bool, error) {
	steers, err := b.loadSteerQueue(ctx, key)
	if err != nil {
		return SteerItem{}, false, err
	}
	item, ok := findSteer(steers, itemID)
	return item, ok, nil
}

func (b *RedisBackend) loadSteerQueue(ctx context.Context, key Key) (steerQueueState, error) {
	if err := b.ensureQueueOpen(); err != nil {
		return steerQueueState{}, err
	}
	if err := validateQueueKey(key); err != nil {
		return steerQueueState{}, err
	}
	return loadRedisJSON[steerQueueState](ctx, b.client, b.steerQueueKey(key))
}

func (b *RedisBackend) ReorderSteer(ctx context.Context, key Key, item, before SteerPendingRef) ([]SteerItem, error) {
	if err := validateQueueKey(key); err != nil {
		return nil, err
	}
	return redisMutate(ctx, b, b.steerQueueKey(key), func(state *steerQueueState, now time.Time) ([]SteerItem, error) {
		items, err := reorderSteerState(state, item, before)
		state.UpdatedAt = now
		return items, err
	})
}

func (b *RedisBackend) UpdateSteer(ctx context.Context, key Key, itemID SteerItemID, payload []byte) (SteerItem, error) {
	if err := validateQueueKey(key); err != nil || itemID == "" || validatePayload(payload) != nil {
		return SteerItem{}, ErrQueueInvalidReference
	}
	return redisMutate(ctx, b, b.steerQueueKey(key), func(state *steerQueueState, now time.Time) (SteerItem, error) {
		return state.edit(itemID, payload, now)
	})
}

func (b *RedisBackend) CancelSteer(ctx context.Context, key Key, itemID SteerItemID) error {
	if err := validateQueueKey(key); err != nil || itemID == "" {
		return ErrQueueInvalidReference
	}
	_, err := redisMutate(ctx, b, b.steerQueueKey(key), func(state *steerQueueState, now time.Time) (struct{}, error) {
		return struct{}{}, state.cancel(itemID, now)
	})
	return err
}

func (b *RedisBackend) ClaimNextSteer(ctx context.Context, handle RunHandle, sealIfEmpty bool) (SteerItem, SteerClaimRef, bool, error) {
	if err := b.ensureQueueOpen(); err != nil {
		return SteerItem{}, SteerClaimRef{}, false, err
	}
	handle = handle.normalized()
	if !handle.valid() || handle.OwnerID == "" || handle.FencingToken <= 0 {
		return SteerItem{}, SteerClaimRef{}, false, ErrQueueInvalidReference
	}
	key := handle.key()
	stateKey, runKey, queueKey := b.stateKey(key), b.runKey(key, handle.RunID), b.steerQueueKey(key)
	var item SteerItem
	var claim SteerClaimRef
	var claimed bool
	// Ownership is watched through the run key, which the finishing owner
	// deletes; the state key is only read for the projected run status.
	err := redisWatch(ctx, b, []string{runKey, queueKey}, func(tx *redis.Tx) error {
		snapshot, ok, err := loadRedisSnapshot(ctx, tx, stateKey)
		if err != nil {
			return err
		}
		ref, refOK, err := loadRedisRunRef(ctx, tx, runKey)
		if err != nil {
			return err
		}
		if !ok || !refOK || !runMatchesHandle(snapshot.CurrentRunView, handle) || ref.FencingToken != handle.FencingToken || ref.OwnerID != handle.OwnerID || ref.Generation != handle.Generation || !isActiveRunStatus(snapshot.CurrentRunView.Status) {
			return ErrRunOwnershipLost
		}
		if !SteerRunAvailable(snapshot.CurrentRunView) {
			return ErrQueueSteerUnsupported
		}
		state, err := loadRedisJSON[steerQueueState](ctx, tx, queueKey)
		if err != nil {
			return err
		}
		now, err := tx.Time(ctx).Result()
		if err != nil {
			return err
		}
		item, claim, claimed = state.claimNext(handle, sealIfEmpty, now.UTC())
		return storeRedisJSON(ctx, tx, queueKey, state, b.stateTTL)
	})
	return item, claim, claimed, err
}

func (b *RedisBackend) ApplySteer(ctx context.Context, key Key, ref SteerClaimRef) error {
	if err := validateSteerClaim(key, ref); err != nil {
		return err
	}
	stateKey, runKey, queueKey := b.stateKey(key), b.runKey(key, ref.RunID), b.steerQueueKey(key)
	_, err := redisMutateWithKeys(ctx, b, []string{runKey, queueKey}, queueKey, func(tx *redis.Tx, state *steerQueueState, now time.Time) (struct{}, error) {
		snapshot, ok, err := loadRedisSnapshot(ctx, tx, stateKey)
		if err != nil {
			return struct{}{}, err
		}
		run, runOK, err := loadRedisRunRef(ctx, tx, runKey)
		if err != nil {
			return struct{}{}, err
		}
		if !ok || !runOK || !runMatchesSteerClaim(snapshot.CurrentRunView, ref) || run.FencingToken != ref.FencingToken || run.OwnerID != ref.OwnerID || run.Generation != ref.Generation {
			return struct{}{}, ErrRunOwnershipLost
		}
		return struct{}{}, state.apply(ref, now)
	})
	return err
}

func (b *RedisBackend) CloseSteerRun(ctx context.Context, key Key, runID string) error {
	runID = strings.TrimSpace(runID)
	if err := validateQueueKey(key); err != nil || runID == "" {
		return ErrQueueInvalidReference
	}
	_, err := redisMutate(ctx, b, b.steerQueueKey(key), func(state *steerQueueState, now time.Time) (struct{}, error) {
		closeSteerRun(state, runID, now)
		return struct{}{}, nil
	})
	return err
}

func (b *RedisBackend) ReleaseSteer(ctx context.Context, key Key, ref SteerClaimRef) error {
	if err := validateSteerClaim(key, ref); err != nil {
		return err
	}
	stateKey, runKey, queueKey := b.stateKey(key), b.runKey(key, ref.RunID), b.steerQueueKey(key)
	_, err := redisMutateWithKeys(ctx, b, []string{runKey, queueKey}, queueKey, func(tx *redis.Tx, state *steerQueueState, now time.Time) (struct{}, error) {
		snapshot, ok, err := loadRedisSnapshot(ctx, tx, stateKey)
		if err != nil {
			return struct{}{}, err
		}
		run, runOK, err := loadRedisRunRef(ctx, tx, runKey)
		if err != nil {
			return struct{}{}, err
		}
		if !ok || !runOK || !runMatchesSteerClaim(snapshot.CurrentRunView, ref) || run.FencingToken != ref.FencingToken || run.OwnerID != ref.OwnerID || run.Generation != ref.Generation {
			return struct{}{}, ErrRunOwnershipLost
		}
		return struct{}{}, state.release(ref, now)
	})
	return err
}

func (b *RedisBackend) steerQueueKey(key Key) string {
	return b.keyPrefix + "steer_queue:" + key.String()
}

var _ LiveQueueBackend = (*RedisBackend)(nil)
