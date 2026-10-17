package activeprompt

import (
	"context"
	"sync"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// Hub fans the events of one in-flight external runtime prompt out to its
// subscribers.
type Hub struct {
	mu     sync.Mutex
	nextID int
	closed bool
	subs   map[int]*subscriber
}

// Subscription is one follower of a Hub.
type Subscription struct {
	sub       *subscriber
	onRelease func()
}

type subscriber struct {
	mu      sync.Mutex
	notify  chan struct{}
	done    chan struct{}
	closed  bool
	pending []native.StreamEvent
}

func newHub() *Hub {
	return &Hub{subs: make(map[int]*subscriber)}
}

func newSubscriber() *subscriber {
	return &subscriber{
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (s *subscriber) enqueue(ev native.StreamEvent) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.pending = append(s.pending, ev)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *subscriber) next(ctx context.Context) (native.StreamEvent, bool, error) {
	for {
		s.mu.Lock()
		if len(s.pending) > 0 {
			ev := s.pending[0]
			copy(s.pending, s.pending[1:])
			s.pending[len(s.pending)-1] = native.StreamEvent{}
			s.pending = s.pending[:len(s.pending)-1]
			s.mu.Unlock()
			return ev, true, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return native.StreamEvent{}, false, nil
		}
		select {
		case <-s.notify:
		case <-s.done:
		case <-ctx.Done():
			return native.StreamEvent{}, false, ctx.Err()
		}
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
}

// Next returns the next event the hub emitted. The bool is false once the hub
// has closed and everything it emitted was read.
func (s *Subscription) Next(ctx context.Context) (native.StreamEvent, bool, error) {
	return s.sub.next(ctx)
}

// Release detaches the subscription from its hub.
func (s *Subscription) Release() {
	s.onRelease()
}

func (h *Hub) subscribe() (*Subscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	id := h.nextID
	h.nextID++
	sub := newSubscriber()
	h.subs[id] = sub
	return &Subscription{
		sub: sub,
		onRelease: func() {
			h.mu.Lock()
			if h.subs[id] == sub {
				delete(h.subs, id)
			}
			h.mu.Unlock()
			sub.close()
		},
	}, true
}

// Emit delivers ev to every current subscriber.
func (h *Hub) Emit(ev native.StreamEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, ch := range h.subs {
		ch.enqueue(ev)
	}
}

func (h *Hub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := h.subs
	h.subs = nil
	h.mu.Unlock()
	for _, sub := range subs {
		sub.close()
	}
}
