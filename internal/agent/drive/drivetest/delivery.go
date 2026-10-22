package drivetest

import (
	"context"
	"sync"

	"github.com/felinics/memoh/internal/agent/agentapi"
)

// RecordingDelivery records what the driver handed it.
type RecordingDelivery struct {
	mu       sync.Mutex
	events   []agentapi.RunEvent
	ended    []agentapi.RunEnded
	settled  int
	GoneAt   int // 1-based Deliver index after which the caller is gone
	delivers int
	// Release, when non-nil, blocks each Deliver until a value arrives.
	Release chan struct{}
}

// Deliver records the event, or reports the caller gone once GoneAt events
// arrived.
func (d *RecordingDelivery) Deliver(_ context.Context, ev agentapi.RunEvent) bool {
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
func (d *RecordingDelivery) Ended(_ context.Context, end agentapi.RunEnded) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ended = append(d.ended, end)
}

// Snapshot is what was delivered so far: the events, the endings and the
// number of settlements.
func (d *RecordingDelivery) Snapshot() ([]agentapi.RunEvent, []agentapi.RunEnded, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agentapi.RunEvent(nil), d.events...), append([]agentapi.RunEnded(nil), d.ended...), d.settled
}
