package application

import (
	"context"
	"strconv"
	"sync"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// memoryQueuedInputs stands in for the queue table where a case only needs the
// drain to be handed a batch. What the table guarantees, the claim, the lease
// and the order, is tested against PostgreSQL; a case that depends on it uses
// the real queue.
type memoryQueuedInputs struct {
	queuedInputStore

	mu       sync.Mutex
	items    []sessionruntime.QueuedInput
	settled  []string
	consumed []string
}

func (m *memoryQueuedInputs) Enqueue(_ context.Context, in sessionruntime.QueueEnqueue) (sessionruntime.QueuedInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item := sessionruntime.QueuedInput{
		ID: "queued-" + strconv.Itoa(len(m.items)+1), BotID: in.Key.BotID, SessionID: in.Key.SessionID,
		Origin: in.Origin, InvocationID: in.InvocationID, MergeKey: in.MergeKey, Payload: in.Payload,
		Position: int64(len(m.items) + 1), State: sessionruntime.QueuedStateQueued,
	}
	m.items = append(m.items, item)
	return item, nil
}

func (*memoryQueuedInputs) Gate(context.Context, sessionruntime.Key, string) (sessionruntime.QueueGate, error) {
	return sessionruntime.QueueGate{}, nil
}

// ClaimBatch hands out every queued input of the session as one batch.
func (m *memoryQueuedInputs) ClaimBatch(_ context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin) (sessionruntime.QueueBatch, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	batch := sessionruntime.QueueBatch{ID: "batch-" + strconv.Itoa(len(m.settled)+1), Key: key, Origin: origin}
	for i := range m.items {
		if m.items[i].State == sessionruntime.QueuedStateQueued && m.items[i].SessionID == key.SessionID && m.items[i].Origin == origin {
			m.items[i].State = sessionruntime.QueuedStateClaimed
			batch.Items = append(batch.Items, m.items[i])
		}
	}
	return batch, len(batch.Items) > 0, nil
}

func (m *memoryQueuedInputs) SettleBatch(_ context.Context, batch sessionruntime.QueueBatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled = append(m.settled, batch.ID)
	for i := range m.items {
		if m.items[i].State == sessionruntime.QueuedStateClaimed {
			m.items[i].State = sessionruntime.QueuedStateConsumed
		}
	}
	return nil
}

func (m *memoryQueuedInputs) Pending(_ context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin) ([]sessionruntime.QueuedInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pending []sessionruntime.QueuedInput
	for _, item := range m.items {
		if item.State == sessionruntime.QueuedStateQueued && item.SessionID == key.SessionID && item.Origin == origin {
			pending = append(pending, item)
		}
	}
	return pending, nil
}

func (m *memoryQueuedInputs) ConsumePromoted(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consumed = append(m.consumed, id)
	return nil
}

func (m *memoryQueuedInputs) consumedIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.consumed...)
}
