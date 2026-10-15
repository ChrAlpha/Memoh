package application

import (
	"context"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// queuedInputStore is the session input queue as the application uses it. The
// PostgreSQL queue is the implementation; the interface is here so a test can
// stand in for the parts of it a case does not exercise.
type queuedInputStore interface {
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

var _ queuedInputStore = (*sessionruntime.QueuedInputs)(nil)

// SetQueuedInputs installs the durable input queue. Without it a turn that
// meets a busy session is refused as busy and nothing is parked.
func (s *Service) SetQueuedInputs(queue *sessionruntime.QueuedInputs) {
	if queue == nil {
		// A typed nil stored in the interface would read as configured.
		return
	}
	s.queue = queue
}
