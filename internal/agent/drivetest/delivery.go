package drivetest

import (
	"context"
	"sync"

	"github.com/felinics/memoh/internal/agent/drive"
)

// RecordingDelivery records what the driver handed it.
type RecordingDelivery struct {
	mu       sync.Mutex
	events   []drive.RunEvent
	ended    []drive.RunEnded
	settled  int
	GoneAt   int // 1-based Deliver index after which the caller is gone
	delivers int
	// Release, when non-nil, blocks each Deliver until a value arrives.
	Release chan struct{}
}

// Deliver records the event, or reports the caller gone once GoneAt events
// arrived.
func (d *RecordingDelivery) Deliver(_ context.Context, ev drive.RunEvent) bool {
	if d.Release != nil {
		<-d.Release
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivers++
	if d.GoneAt > 0 && d.delivers >= d.GoneAt {
		return false
	}
	d.events = append(d.events, ev)
	return true
}

// Settle counts the settlement.
func (d *RecordingDelivery) Settle() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settled++
}

// Ended records how the run ended.
func (d *RecordingDelivery) Ended(_ context.Context, end drive.RunEnded) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ended = append(d.ended, end)
}

// Snapshot is what was delivered so far: the events, the endings and the
// number of settlements.
func (d *RecordingDelivery) Snapshot() ([]drive.RunEvent, []drive.RunEnded, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]drive.RunEvent(nil), d.events...), append([]drive.RunEnded(nil), d.ended...), d.settled
}
