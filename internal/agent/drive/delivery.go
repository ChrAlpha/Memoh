package drive

import (
	"context"
	"sync"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// reportedAssetsSource is a delivery end that collects asset references for
// the caller's own outbound path — the attachments a channel ingested, the
// audio it synthesized — so the run can link them to the reply they belong to.
// It is separate from the run's own collector because those references exist
// only once the caller has seen the events.
type reportedAssetsSource interface {
	ReportedAssets() []runreq.OutboundAssetRef
}

// NoDelivery is the delivery end of a run nobody follows: a schedule fire, an
// interrupted-session resume, a decision continuation.
type NoDelivery struct{}

func (NoDelivery) Deliver(context.Context, agentapi.RunEvent) bool { return true }

func (NoDelivery) Settle() {}

func (NoDelivery) Ended(context.Context, agentapi.RunEnded) {}

// deliveryQueue feeds a RunDelivery from its own goroutine, in the order the
// driver enqueued events, so a slow caller never blocks publication.
//
// Growth is bounded by buffered bytes rather than by a timer: once the pending
// bytes reach the limit, enqueueing waits for the caller to catch up. A caller
// that is still reading therefore never loses an event, and a caller that is
// gone is detected by its own signal — Deliver returning false, which is what
// the caller's context ending makes it do — not by a timeout, which would
// truncate a reply from a caller that is merely slow.
type deliveryQueue struct {
	ctx      context.Context
	delivery agentapi.RunDelivery
	limit    int

	mu      sync.Mutex
	cond    *sync.Cond
	pending []agentapi.RunEvent
	bytes   int
	closed  bool
	// gone is set once the caller reported it is no longer reading. The queue
	// then drops what is pending and enqueues nothing further.
	gone bool
	done chan struct{}
}

// deliveryQueueLimit is the buffered-byte soft limit at which enqueueing starts
// waiting for the delivery end.
const deliveryQueueLimit = 8 << 20

func startDeliveryQueue(ctx context.Context, delivery agentapi.RunDelivery) *deliveryQueue {
	if delivery == nil {
		delivery = NoDelivery{}
	}
	q := &deliveryQueue{
		ctx:      ctx,
		delivery: delivery,
		limit:    deliveryQueueLimit,
		done:     make(chan struct{}),
	}
	q.cond = sync.NewCond(&q.mu)
	go q.run()
	return q
}

// enqueue hands one event to the delivery end. It returns false once the caller
// is gone, so the driver stops preparing events for it.
func (q *deliveryQueue) enqueue(ev agentapi.RunEvent) bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.gone && !q.closed && q.bytes >= q.limit {
		q.cond.Wait()
	}
	if q.gone || q.closed {
		return !q.gone
	}
	q.pending = append(q.pending, ev)
	q.bytes += len(ev.Payload)
	q.cond.Broadcast()
	return true
}

// closeAndWait ends the queue and returns once every pending event has been
// handed to the caller or the caller is gone.
func (q *deliveryQueue) closeAndWait() {
	if q == nil {
		return
	}
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
	<-q.done
}

func (q *deliveryQueue) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed && !q.gone {
			q.cond.Wait()
		}
		if q.gone {
			q.pending = nil
			q.bytes = 0
			q.mu.Unlock()
			return
		}
		if len(q.pending) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		ev := q.pending[0]
		q.pending = q.pending[1:]
		q.bytes -= len(ev.Payload)
		q.cond.Broadcast()
		q.mu.Unlock()

		if !q.deliver(ev) {
			q.mu.Lock()
			q.gone = true
			q.pending = nil
			q.bytes = 0
			q.cond.Broadcast()
			q.mu.Unlock()
			return
		}
	}
}

func (q *deliveryQueue) deliver(ev agentapi.RunEvent) (ok bool) {
	defer func() {
		// A caller that closed its own end must not take the run down with it.
		if recover() != nil {
			ok = false
		}
	}()
	return q.delivery.Deliver(q.ctx, ev)
}
