package sessionruntime

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

func (m *Manager) liveQueueBackend() (LiveQueueBackend, error) {
	if m == nil || m.backend == nil {
		return nil, ErrManagerClosed
	}
	queue, ok := m.backend.(LiveQueueBackend)
	if !ok {
		return nil, ErrLiveQueueUnavailable
	}
	return queue, nil
}

// EnableSteer advertises an actual execution consumer, not just an allocated
// channel. Other instances therefore reject queues aimed at old/unsupported owners.
func (m *Manager) EnableSteer(ctx context.Context, handle RunHandle) error {
	return m.setSteerEnabled(ctx, handle, true)
}

func (m *Manager) DisableSteer(ctx context.Context, handle RunHandle) error {
	return m.setSteerEnabled(ctx, handle, false)
}

func (m *Manager) setSteerEnabled(ctx context.Context, handle RunHandle, enabled bool) error {
	if m.SteerWake(handle) == nil {
		return ErrRunOwnershipLost
	}
	_, _, err := m.updateActiveAndPublish(ctx, handle, func(snapshot Snapshot, now time.Time) (Snapshot, bool, error) {
		run := snapshot.CurrentRunView
		if !runMatchesHandle(run, handle) || !m.runOwnerMatches(run) ||
			(run.Status != RunStatusRunning && run.Status != RunStatusWaitingDecision) {
			return snapshot, false, ErrRunOwnershipLost
		}
		if run.SteerSupported == enabled {
			return snapshot, false, nil
		}
		run.SteerSupported = enabled
		run.UpdatedAt = now
		snapshot.Seq++
		snapshot.UpdatedAt = now
		return snapshot, true, nil
	}, func(snapshot Snapshot) RuntimeDelta { return RuntimeDelta{CurrentRunView: snapshot.CurrentRunView} })
	return err
}

func (m *Manager) EnqueueSteer(ctx context.Context, key Key, itemID, invocationID string, payload []byte) (SteerItem, error) {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return SteerItem{}, err
	}
	item, err := queue.EnqueueSteer(ctx, key, itemID, invocationID, payload)
	if err == nil && item.Status == QueueAccepted {
		m.notifySteer(ctx, key, item.TargetRunID)
	}
	return item, err
}

func (m *Manager) PendingSteers(ctx context.Context, key Key, limit int) ([]SteerItem, error) {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return nil, err
	}
	return queue.PendingSteers(ctx, key, limit)
}

func (m *Manager) LookupSteer(ctx context.Context, key Key, itemID SteerItemID) (SteerItem, bool, error) {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return SteerItem{}, false, err
	}
	return queue.LookupSteer(ctx, key, itemID)
}

func (m *Manager) ReorderSteer(ctx context.Context, key Key, item, before SteerPendingRef) ([]SteerItem, error) {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return nil, err
	}
	return queue.ReorderSteer(ctx, key, item, before)
}

func (m *Manager) UpdateSteer(ctx context.Context, key Key, itemID SteerItemID, payload []byte) (SteerItem, error) {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return SteerItem{}, err
	}
	return queue.UpdateSteer(ctx, key, itemID, payload)
}

func (m *Manager) CancelSteer(ctx context.Context, key Key, itemID SteerItemID) error {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return err
	}
	return queue.CancelSteer(ctx, key, itemID)
}

// SteerWake is an owner-local, coalesced notification. The queue remains the
// source of truth; neither duplicate notifications nor a stale wake apply input.
func (m *Manager) SteerWake(handle RunHandle) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctrl := m.controls[scopedRunControlKey(handle.BotID, handle.SessionID, handle.RunID)]
	if ctrl == nil || ctrl.generation != handle.Generation {
		return nil
	}
	if ctrl.steerWake == nil {
		ctrl.steerWake = make(chan struct{}, 1)
	}
	return ctrl.steerWake
}

func (m *Manager) wakeSteer(ctrl *runControl) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controls[ctrl.key()] != ctrl || ctrl.steerWake == nil {
		return
	}
	select {
	case ctrl.steerWake <- struct{}{}:
	default:
	}
}

func (m *Manager) notifySteer(ctx context.Context, key Key, runID string) {
	// Input is already accepted. Finish the bounded notification independently
	// of the HTTP connection, and never report a delivery error as a rejected input.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.commandTimeout())
	defer cancel()
	snapshot, ok, err := m.backend.Load(ctx, key)
	if err != nil || !ok || snapshot.CurrentRunView == nil || snapshot.CurrentRunView.RunID != runID {
		return
	}
	run := snapshot.CurrentRunView
	now, err := m.backend.Now(ctx)
	if err != nil {
		return
	}
	cmd := Command{
		Type: CommandSteerWake, ID: uuid.NewString(), BotID: key.BotID,
		SessionID: key.SessionID, RunID: runID, Generation: run.Generation,
		FencingToken: run.FencingToken, CreatedAt: now, ExpiresAt: now.Add(m.commandTimeout()),
	}
	if m.runOwnerMatches(run) {
		err = m.applyRoutedCommand(ctx, cmd)
	} else if m.distributed != nil {
		err = m.dispatchRemoteCommand(ctx, run.OwnerID, cmd)
	}
	if err != nil {
		m.logger.WarnContext(ctx, "notify accepted steer failed", slog.String("run_id", runID), slog.Any("error", err))
	}
}

func (m *Manager) ClaimNextSteer(ctx context.Context, handle RunHandle, sealIfEmpty bool) (SteerItem, SteerClaimRef, bool, error) {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return SteerItem{}, SteerClaimRef{}, false, err
	}
	return queue.ClaimNextSteer(ctx, handle, sealIfEmpty)
}

func (m *Manager) ApplySteer(ctx context.Context, key Key, ref SteerClaimRef) error {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return err
	}
	return queue.ApplySteer(ctx, key, ref)
}

func (m *Manager) ReleaseSteer(ctx context.Context, key Key, ref SteerClaimRef) error {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return err
	}
	return queue.ReleaseSteer(ctx, key, ref)
}

func (m *Manager) CloseSteerRun(ctx context.Context, key Key, runID string) error {
	queue, err := m.liveQueueBackend()
	if err != nil {
		return err
	}
	return queue.CloseSteerRun(ctx, key, runID)
}
