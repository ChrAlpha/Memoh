package pending

import (
	"context"
	"time"

	"github.com/felinics/memoh/internal/agent/persist"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// Store is the session input queue as pending uses it. The PostgreSQL queue is
// the implementation; the interface is here so a test can stand in for the
// parts of it a case does not exercise.
type Store interface {
	Enqueue(context.Context, sessionruntime.QueueEnqueue) (sessionruntime.QueuedInput, error)
	Gate(ctx context.Context, key sessionruntime.Key, invocationID string) (sessionruntime.QueueGate, error)
	Pending(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin) ([]sessionruntime.QueuedInput, error)
	Reorder(ctx context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin, item, before string) ([]sessionruntime.QueuedInput, error)
	Edit(ctx context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin, id string, payload []byte) (sessionruntime.QueuedInput, error)
	Cancel(ctx context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin, id string) error
	ClaimBatch(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin) (sessionruntime.QueueBatch, bool, error)
	SettleBatch(context.Context, sessionruntime.QueueBatch) error
	ReleaseBatch(context.Context, sessionruntime.QueueBatch) error
	Reject(ctx context.Context, id, errorCode string) error
	Drainable(context.Context, sessionruntime.QueueOrigin) ([]sessionruntime.Key, error)
	BeginPromotion(ctx context.Context, key sessionruntime.Key, id string) (sessionruntime.QueuedPromotion, error)
	CompletePromotion(ctx context.Context, id, token string) error
	AbortPromotion(ctx context.Context, id, token string) error
	ConsumePromoted(ctx context.Context, id string) error
	RequeuePromoted(ctx context.Context, id string) error
	Promotions(context.Context, sessionruntime.Key) ([]sessionruntime.QueuedInput, error)
	StalePromotions(context.Context, time.Time) ([]sessionruntime.QueuedInput, error)
	PurgeSettled(context.Context, time.Time) (int64, error)
}

var _ Store = (*sessionruntime.QueuedInputs)(nil)

// The queue's store is what records that a run applied a promoted input, so it
// is the persist component's port for it.
var _ persist.PromotedInputs = Store(nil)

// noQueue is the store of a deployment that has no database for the queue.
// Nothing is queued there, so nothing waits behind or is claimable: a gate is
// open, a claim finds nothing, and no promotion exists. Every operation that
// would put something in or take something out of the queue is unavailable.
type noQueue struct{}

func (noQueue) Enqueue(context.Context, sessionruntime.QueueEnqueue) (sessionruntime.QueuedInput, error) {
	return sessionruntime.QueuedInput{}, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Gate(context.Context, sessionruntime.Key, string) (sessionruntime.QueueGate, error) {
	return sessionruntime.QueueGate{}, nil
}

func (noQueue) Pending(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin) ([]sessionruntime.QueuedInput, error) {
	return nil, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Reorder(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin, string, string) ([]sessionruntime.QueuedInput, error) {
	return nil, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Edit(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin, string, []byte) (sessionruntime.QueuedInput, error) {
	return sessionruntime.QueuedInput{}, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Cancel(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin, string) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) ClaimBatch(context.Context, sessionruntime.Key, sessionruntime.QueueOrigin) (sessionruntime.QueueBatch, bool, error) {
	return sessionruntime.QueueBatch{}, false, nil
}

func (noQueue) SettleBatch(context.Context, sessionruntime.QueueBatch) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) ReleaseBatch(context.Context, sessionruntime.QueueBatch) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Reject(context.Context, string, string) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Drainable(context.Context, sessionruntime.QueueOrigin) ([]sessionruntime.Key, error) {
	return nil, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) BeginPromotion(context.Context, sessionruntime.Key, string) (sessionruntime.QueuedPromotion, error) {
	return sessionruntime.QueuedPromotion{}, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) CompletePromotion(context.Context, string, string) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) AbortPromotion(context.Context, string, string) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) ConsumePromoted(context.Context, string) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) RequeuePromoted(context.Context, string) error {
	return sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) Promotions(context.Context, sessionruntime.Key) ([]sessionruntime.QueuedInput, error) {
	return nil, nil
}

func (noQueue) StalePromotions(context.Context, time.Time) ([]sessionruntime.QueuedInput, error) {
	return nil, sessionruntime.ErrLiveQueueUnavailable
}

func (noQueue) PurgeSettled(context.Context, time.Time) (int64, error) {
	return 0, sessionruntime.ErrLiveQueueUnavailable
}
