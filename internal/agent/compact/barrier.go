package compact

import (
	"strings"
	"sync"

	"github.com/felinics/memoh/internal/agent/runreq"
)

type sessionGate struct {
	lock    sync.RWMutex
	refs    int
	readers map[string]map[*sessionReader]struct{}
}

type sessionReader struct {
	gate   *sessionGate
	mu     sync.Mutex
	held   bool
	closed bool
}

// Defer marks the run as reading the session's history until the returned
// function is called, so a compaction of the session waits for it. Runs of the
// same session do not wait for each other.
func (c *Compactor) Defer(botID, sessionID, runID string) func() {
	gate, key := c.acquire(botID, sessionID)
	if gate == nil {
		return func() {}
	}
	gate.lock.RLock()
	reader := &sessionReader{gate: gate, held: true}
	runID = strings.TrimSpace(runID)
	if runID != "" {
		c.mu.Lock()
		if gate.readers == nil {
			gate.readers = make(map[string]map[*sessionReader]struct{})
		}
		if gate.readers[runID] == nil {
			gate.readers[runID] = make(map[*sessionReader]struct{})
		}
		gate.readers[runID][reader] = struct{}{}
		c.mu.Unlock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			reader.close()
			if runID != "" {
				c.mu.Lock()
				delete(gate.readers[runID], reader)
				if len(gate.readers[runID]) == 0 {
					delete(gate.readers, runID)
				}
				c.mu.Unlock()
			}
			c.release(key, gate)
		})
	}
}

func (reader *sessionReader) suspend() bool {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed || !reader.held {
		return false
	}
	reader.gate.lock.RUnlock()
	reader.held = false
	return true
}

func (reader *sessionReader) resume() {
	reader.gate.lock.RLock()
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		reader.gate.lock.RUnlock()
		return
	}
	reader.held = true
}

func (reader *sessionReader) close() {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return
	}
	reader.closed = true
	if reader.held {
		reader.gate.lock.RUnlock()
		reader.held = false
	}
}

func (c *Compactor) enter(botID, sessionID string) func() {
	gate, key := c.acquire(botID, sessionID)
	if gate == nil {
		return func() {}
	}
	gate.lock.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.lock.Unlock()
			c.release(key, gate)
		})
	}
}

func (c *Compactor) enterForRun(botID, sessionID, runID string) func() {
	gate, key := c.acquire(botID, sessionID)
	if gate == nil {
		return func() {}
	}
	readers := c.readersOf(gate, runID)
	suspended := make([]*sessionReader, 0, len(readers))
	for _, reader := range readers {
		if reader.suspend() {
			suspended = append(suspended, reader)
		}
	}
	gate.lock.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.lock.Unlock()
			for _, reader := range suspended {
				reader.resume()
			}
			c.release(key, gate)
		})
	}
}

// await blocks while a compaction pass holds the session's
// barrier and reports whether it had to wait. The run's own readers step aside
// for the wait, as they do for its own compaction, so a pass queued behind
// them cannot deadlock against this call.
func (c *Compactor) await(botID, sessionID, runID string) bool {
	gate, key := c.acquire(botID, sessionID)
	if gate == nil {
		return false
	}
	defer c.release(key, gate)
	readers := c.readersOf(gate, runID)
	suspended := make([]*sessionReader, 0, len(readers))
	for _, reader := range readers {
		if reader.suspend() {
			suspended = append(suspended, reader)
		}
	}
	waited := !gate.lock.TryLock()
	if waited {
		gate.lock.Lock()
	}
	gate.lock.Unlock()
	for _, reader := range suspended {
		reader.resume()
	}
	return waited
}

func (c *Compactor) readersOf(gate *sessionGate, runID string) []*sessionReader {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	readers := make([]*sessionReader, 0, len(gate.readers[runID]))
	for reader := range gate.readers[runID] {
		readers = append(readers, reader)
	}
	return readers
}

func (c *Compactor) acquire(botID, sessionID string) (*sessionGate, string) {
	botID = strings.TrimSpace(botID)
	sessionID = strings.TrimSpace(sessionID)
	if botID == "" || sessionID == "" {
		return nil, ""
	}
	key := runreq.SessionTurnKey(botID, sessionID)
	c.mu.Lock()
	defer c.mu.Unlock()
	gate := c.gates[key]
	if gate == nil {
		gate = &sessionGate{}
		c.gates[key] = gate
	}
	gate.refs++
	return gate, key
}

func (c *Compactor) release(key string, gate *sessionGate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gate.refs--
	if gate.refs == 0 && c.gates[key] == gate {
		delete(c.gates, key)
	}
}
