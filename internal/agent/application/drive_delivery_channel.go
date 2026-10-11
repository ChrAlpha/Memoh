package application

import (
	"context"
	"encoding/json"
)

// channelDelivery hands a run's events to a caller that reads them as JSON
// from a channel: an answer submitted where no session runtime routes it, so
// the caller that submitted it streams the continuation itself.
//
// The channel belongs to the caller, which closes it once the run returns; a
// delivery end that closed it would race the caller's own close.
type channelDelivery struct {
	ch chan<- json.RawMessage
}

// Deliver writes one event, or reports the caller gone once its context ends.
func (d channelDelivery) Deliver(ctx context.Context, ev RunEvent) bool {
	select {
	case d.ch <- ev.Payload:
		return true
	case <-ctx.Done():
		return false
	}
}

func (channelDelivery) Settle()                         {}
func (channelDelivery) Ended(context.Context, RunEnded) {}
