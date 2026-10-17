package drive

import (
	"context"
	"encoding/json"
)

// ChannelDelivery hands a run's events to a caller that reads them as JSON
// from a channel: an answer submitted where no session runtime routes it, so
// the caller that submitted it streams the continuation itself.
//
// The channel belongs to the caller, which closes it once the run returns; a
// delivery end that closed it would race the caller's own close.
type ChannelDelivery struct {
	ch chan<- json.RawMessage
}

// NewChannelDelivery delivers the events of a run to ch. The caller closes ch.
func NewChannelDelivery(ch chan<- json.RawMessage) ChannelDelivery {
	return ChannelDelivery{ch: ch}
}

// Deliver writes one event, or reports the caller gone once its context ends.
func (d ChannelDelivery) Deliver(ctx context.Context, ev RunEvent) bool {
	select {
	case d.ch <- ev.Payload:
		return true
	case <-ctx.Done():
		return false
	}
}

func (ChannelDelivery) Settle() {}

func (ChannelDelivery) Ended(context.Context, RunEnded) {}
