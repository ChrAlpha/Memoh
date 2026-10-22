package runreq

import (
	"context"
	"sync"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// ActiveTurnTracker keeps the database open until canceled turns have finished
// their application-side persistence. A run's durable interruption is recorded
// before draining begins; this only waits for the old process to stop writing.
type ActiveTurnTracker struct {
	mu       sync.Mutex
	active   int
	draining bool
	done     chan struct{}
}

// Begin registers a turn and returns its release function. It fails once draining has started.
func (t *ActiveTurnTracker) Begin() (func(), error) {
	t.mu.Lock()
	if t.draining {
		t.mu.Unlock()
		return nil, sessionruntime.ErrManagerClosed
	}
	t.active++
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			t.active--
			if t.draining && t.active == 0 && t.done != nil {
				close(t.done)
			}
			t.mu.Unlock()
		})
	}, nil
}

// Drain refuses new turns and waits until the registered ones have released or ctx ends.
func (t *ActiveTurnTracker) Drain(ctx context.Context) error {
	t.mu.Lock()
	t.draining = true
	if t.active == 0 {
		t.mu.Unlock()
		return nil
	}
	if t.done == nil {
		t.done = make(chan struct{})
	}
	done := t.done
	t.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
