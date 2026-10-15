package sessionruntime

import (
	"time"

	"github.com/google/uuid"
)

// Steer queue transitions have one implementation. Backends supply
// serialization, ownership validation and their authoritative clock, then store
// the result.
func (state *steerQueueState) claimNext(handle RunHandle, sealIfEmpty bool, now time.Time) (SteerItem, SteerClaimRef, bool) {
	for i := range state.Items {
		item := &state.Items[i]
		if item.Status == QueueClaimed && item.Claim != nil && item.Claim.RunID == handle.RunID {
			if advanceSteerClaim(item, handle) {
				state.UpdatedAt = now
			}
			return cloneSteerItem(*item), *item.Claim, true
		}
	}
	best := -1
	for i := range state.Items {
		if state.Items[i].Status == QueueAccepted && state.Items[i].TargetRunID == handle.RunID &&
			(best < 0 || state.Items[i].Position < state.Items[best].Position) {
			best = i
		}
	}
	if best < 0 {
		if sealIfEmpty {
			state.ClosedRunID = handle.RunID
			state.UpdatedAt = now
		}
		return SteerItem{}, SteerClaimRef{}, false
	}
	claim := SteerClaimRef{ItemID: state.Items[best].ID, RunID: handle.RunID, OwnerID: handle.OwnerID, Generation: handle.Generation, FencingToken: handle.FencingToken, ClaimToken: uuid.NewString()}
	state.Items[best].Status = QueueClaimed
	state.Items[best].Claim = &claim
	state.UpdatedAt = now
	return cloneSteerItem(state.Items[best]), claim, true
}

func (state *steerQueueState) apply(ref SteerClaimRef, now time.Time) error {
	for i := range state.Items {
		claim := state.Items[i].Claim
		if state.Items[i].ID == ref.ItemID && state.Items[i].Status == QueueClaimed && claim != nil && *claim == ref {
			state.Items[i].Status = QueueApplied
			state.UpdatedAt = now
			state.compact()
			return nil
		}
	}
	return ErrQueueInvalidReference
}

func (state *steerQueueState) release(ref SteerClaimRef, now time.Time) error {
	for i := range state.Items {
		claim := state.Items[i].Claim
		if state.Items[i].ID == ref.ItemID && state.Items[i].Status == QueueClaimed && claim != nil && *claim == ref {
			state.Items[i].Status = QueueAccepted
			state.Items[i].Claim = nil
			state.UpdatedAt = now
			return nil
		}
	}
	return ErrQueueInvalidReference
}

func (state *steerQueueState) edit(itemID SteerItemID, payload []byte, now time.Time) (SteerItem, error) {
	for i := range state.Items {
		if state.Items[i].ID == itemID && state.Items[i].Status == QueueAccepted {
			state.Items[i].Payload = append([]byte(nil), payload...)
			state.UpdatedAt = now
			return cloneSteerItem(state.Items[i]), nil
		}
	}
	return SteerItem{}, ErrQueueNotPending
}

func (state *steerQueueState) cancel(itemID SteerItemID, now time.Time) error {
	for i := range state.Items {
		if state.Items[i].ID == itemID && state.Items[i].Status == QueueAccepted {
			state.Items[i].Status = QueueCanceled
			state.UpdatedAt = now
			state.compact()
			return nil
		}
	}
	return ErrQueueNotPending
}

func (state *steerQueueState) enqueue(key Key, itemID, invocationID, runID string, payload []byte, now time.Time) (SteerItem, error) {
	if countPendingSteers(*state) >= MaxPendingQueueItems {
		return SteerItem{}, ErrQueueCapacityExceeded
	}
	item := SteerItem{
		ID: SteerItemID(itemID), BotID: key.BotID, SessionID: key.SessionID,
		TargetRunID: runID, InvocationID: invocationID, Payload: append([]byte(nil), payload...),
		Status: QueueAccepted, Position: nextSteerPosition(*state), CreatedAt: now,
	}
	state.Items = append(state.Items, item)
	state.UpdatedAt = now
	if state.ClosedRunID != "" && state.ClosedRunID != runID {
		state.ClosedRunID = ""
	}
	return cloneSteerItem(item), nil
}
