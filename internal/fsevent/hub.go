// Package fsevent fans out debounced per-bot workspace filesystem change
// notifications from host-side mutation chokepoints (fs API, agent tools,
// workspace watchers) to live UI subscribers.
package fsevent

import (
	"sync"
	"time"
)

// DefaultWindow is the trailing coalescing window applied to publishes.
const DefaultWindow = 200 * time.Millisecond

// maxBatchPaths bounds one delivery's path list; larger batches collapse to a
// wildcard (nil) so payloads stay small and consumers fall back to a full
// refresh.
const maxBatchPaths = 16

type pendingBatch struct {
	paths    map[string]struct{}
	wildcard bool
}

// Hub coalesces Publish calls per bot within a trailing window and delivers
// one batch to every subscriber of that bot. A nil paths slice means
// "unknown scope" and turns the whole batch into a wildcard.
type Hub struct {
	mu      sync.Mutex
	window  time.Duration
	nextID  int
	subs    map[string]map[int]func([]string)
	pending map[string]*pendingBatch
}

func NewHub(window time.Duration) *Hub {
	if window <= 0 {
		window = DefaultWindow
	}
	return &Hub{
		window:  window,
		subs:    make(map[string]map[int]func([]string)),
		pending: make(map[string]*pendingBatch),
	}
}

// Publish records a change for botID. paths carries the touched absolute
// paths when known; nil means unknown scope (wildcard).
func (h *Hub) Publish(botID string, paths []string) {
	if botID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	batch, ok := h.pending[botID]
	if !ok {
		batch = &pendingBatch{paths: make(map[string]struct{})}
		h.pending[botID] = batch
		time.AfterFunc(h.window, func() { h.flush(botID) })
	}
	if batch.wildcard {
		return
	}
	if paths == nil {
		batch.wildcard = true
		batch.paths = nil
		return
	}
	for _, p := range paths {
		batch.paths[p] = struct{}{}
	}
	if len(batch.paths) > maxBatchPaths {
		batch.wildcard = true
		batch.paths = nil
	}
}

func (h *Hub) flush(botID string) {
	h.mu.Lock()
	batch, ok := h.pending[botID]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.pending, botID)
	var delivery []string
	if !batch.wildcard {
		delivery = make([]string, 0, len(batch.paths))
		for p := range batch.paths {
			delivery = append(delivery, p)
		}
	}
	fns := make([]func([]string), 0, len(h.subs[botID]))
	for _, fn := range h.subs[botID] {
		fns = append(fns, fn)
	}
	h.mu.Unlock()
	for _, fn := range fns {
		fn(delivery)
	}
}

// Subscribe registers fn for botID deliveries and returns an idempotent
// cancel. fn runs on the hub's flush goroutine and must not block.
func (h *Hub) Subscribe(botID string, fn func(paths []string)) (cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	if h.subs[botID] == nil {
		h.subs[botID] = make(map[int]func([]string))
	}
	h.subs[botID][id] = fn
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if subs, ok := h.subs[botID]; ok {
			delete(subs, id)
			if len(subs) == 0 {
				delete(h.subs, botID)
			}
		}
	}
}
