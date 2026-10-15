package sessionruntime

import (
	"context"
	"strings"
	"time"
)

func (b *MemoryBackend) purgeLiveQueuesLocked(now time.Time) {
	for key, state := range b.steerQueues {
		if !state.UpdatedAt.IsZero() && now.Sub(state.UpdatedAt) >= b.stateTTL {
			delete(b.steerQueues, key)
		}
	}
}

func (b *MemoryBackend) liveSnapshotLocked(key Key, now time.Time) (Snapshot, bool) {
	b.purgeExpiredLocked(now)
	snapshot, ok := b.snapshots[key.String()]
	return snapshot, ok
}

func (b *MemoryBackend) EnqueueSteer(ctx context.Context, key Key, itemID, invocationID string, payload []byte) (SteerItem, error) {
	if err := contextError(ctx); err != nil {
		return SteerItem{}, err
	}
	if err := validateQueueKey(key); err != nil || strings.TrimSpace(itemID) == "" || strings.TrimSpace(invocationID) == "" || len(payload) == 0 {
		return SteerItem{}, ErrQueueInvalidReference
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return SteerItem{}, ErrLiveQueueUnavailable
	}
	now := time.Now().UTC()
	b.purgeLiveQueuesLocked(now)
	state := b.steerQueues[key.String()]
	if item, ok, err := replaySteer(state, invocationID, payload); ok {
		return item, err
	}
	snapshot, ok := b.liveSnapshotLocked(key, now)
	run, active := activeRun(snapshot, ok)
	if !active || state.ClosedRunID == run.RunID {
		return SteerItem{}, ErrQueueNoActiveRun
	}
	if !SteerRunAvailable(run) {
		return SteerItem{}, ErrQueueSteerUnsupported
	}
	item, err := state.enqueue(key, itemID, invocationID, run.RunID, payload, now)
	if err == nil {
		b.steerQueues[key.String()] = state
	}
	return item, err
}

func (b *MemoryBackend) PendingSteers(ctx context.Context, key Key, limit int) ([]SteerItem, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateQueueKey(key); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrLiveQueueUnavailable
	}
	b.purgeLiveQueuesLocked(time.Now().UTC())
	return pendingSteers(b.steerQueues[key.String()], limit), nil
}

func (b *MemoryBackend) LookupSteer(ctx context.Context, key Key, itemID SteerItemID) (SteerItem, bool, error) {
	if err := contextError(ctx); err != nil {
		return SteerItem{}, false, err
	}
	if err := validateQueueKey(key); err != nil {
		return SteerItem{}, false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return SteerItem{}, false, ErrLiveQueueUnavailable
	}
	b.purgeLiveQueuesLocked(time.Now().UTC())
	item, ok := findSteer(b.steerQueues[key.String()], itemID)
	return item, ok, nil
}

func (b *MemoryBackend) ReorderSteer(ctx context.Context, key Key, item, before SteerPendingRef) ([]SteerItem, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrLiveQueueUnavailable
	}
	now := time.Now().UTC()
	b.purgeLiveQueuesLocked(now)
	if err := validateQueueKey(key); err != nil {
		return nil, err
	}
	state := b.steerQueues[key.String()]
	items, err := reorderSteerState(&state, item, before)
	if err != nil {
		return nil, err
	}
	state.UpdatedAt = now
	b.steerQueues[key.String()] = state
	return items, nil
}

func (b *MemoryBackend) UpdateSteer(ctx context.Context, key Key, itemID SteerItemID, payload []byte) (SteerItem, error) {
	if err := contextError(ctx); err != nil {
		return SteerItem{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return SteerItem{}, ErrLiveQueueUnavailable
	}
	if err := validateQueueKey(key); err != nil || itemID == "" || validatePayload(payload) != nil {
		return SteerItem{}, ErrQueueInvalidReference
	}
	state := b.steerQueues[key.String()]
	item, err := state.edit(itemID, payload, time.Now().UTC())
	if err == nil {
		b.steerQueues[key.String()] = state
	}
	return item, err
}

func (b *MemoryBackend) CancelSteer(ctx context.Context, key Key, itemID SteerItemID) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrLiveQueueUnavailable
	}
	if err := validateQueueKey(key); err != nil || itemID == "" {
		return ErrQueueInvalidReference
	}
	state := b.steerQueues[key.String()]
	err := state.cancel(itemID, time.Now().UTC())
	if err == nil {
		b.steerQueues[key.String()] = state
	}
	return err
}

func (b *MemoryBackend) CloseSteerRun(ctx context.Context, key Key, runID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	runID = strings.TrimSpace(runID)
	if err := validateQueueKey(key); err != nil || runID == "" {
		return ErrQueueInvalidReference
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrLiveQueueUnavailable
	}
	state, ok := b.steerQueues[key.String()]
	if !ok {
		// No steer was ever admitted for this session; there is nothing to
		// seal because a later run has its own run ID.
		return nil
	}
	if closeSteerRun(&state, runID, time.Now().UTC()) {
		state.compact()
		b.steerQueues[key.String()] = state
	}
	return nil
}

func (b *MemoryBackend) ClaimNextSteer(ctx context.Context, handle RunHandle, sealIfEmpty bool) (SteerItem, SteerClaimRef, bool, error) {
	if err := contextError(ctx); err != nil {
		return SteerItem{}, SteerClaimRef{}, false, err
	}
	handle = handle.normalized()
	if !handle.valid() || handle.OwnerID == "" || handle.FencingToken <= 0 {
		return SteerItem{}, SteerClaimRef{}, false, ErrQueueInvalidReference
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return SteerItem{}, SteerClaimRef{}, false, ErrLiveQueueUnavailable
	}
	now := time.Now().UTC()
	snapshot, ok := b.liveSnapshotLocked(handle.key(), now)
	if !ok || !runMatchesHandle(snapshot.CurrentRunView, handle) || !runViewOwnedBy(snapshot.CurrentRunView, handle.OwnerID) || !isActiveRunStatus(snapshot.CurrentRunView.Status) {
		return SteerItem{}, SteerClaimRef{}, false, ErrRunOwnershipLost
	}
	if !SteerRunAvailable(snapshot.CurrentRunView) {
		return SteerItem{}, SteerClaimRef{}, false, ErrQueueSteerUnsupported
	}
	state := b.steerQueues[handle.key().String()]
	item, claim, claimed := state.claimNext(handle, sealIfEmpty, now)
	b.steerQueues[handle.key().String()] = state
	return item, claim, claimed, nil
}

func (b *MemoryBackend) ApplySteer(ctx context.Context, key Key, ref SteerClaimRef) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrLiveQueueUnavailable
	}
	if err := validateSteerClaim(key, ref); err != nil {
		return err
	}
	snapshot, ok := b.liveSnapshotLocked(key, time.Now().UTC())
	if !ok || !runMatchesSteerClaim(snapshot.CurrentRunView, ref) {
		return ErrRunOwnershipLost
	}
	state := b.steerQueues[key.String()]
	err := state.apply(ref, time.Now().UTC())
	if err == nil {
		b.steerQueues[key.String()] = state
	}
	return err
}

func (b *MemoryBackend) ReleaseSteer(ctx context.Context, key Key, ref SteerClaimRef) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrLiveQueueUnavailable
	}
	if err := validateSteerClaim(key, ref); err != nil {
		return err
	}
	snapshot, ok := b.liveSnapshotLocked(key, time.Now().UTC())
	if !ok || !runMatchesSteerClaim(snapshot.CurrentRunView, ref) {
		return ErrRunOwnershipLost
	}
	state := b.steerQueues[key.String()]
	err := state.release(ref, time.Now().UTC())
	if err == nil {
		b.steerQueues[key.String()] = state
	}
	return err
}

var _ LiveQueueBackend = (*MemoryBackend)(nil)
