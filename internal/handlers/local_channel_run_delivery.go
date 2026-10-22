package handlers

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// wsRunDelivery is the initiating socket's end of a Web run.
//
// The socket is not a special case for the run's output. It reads the run the
// way every other subscriber of the session does, through the snapshot and
// deltas of the session it subscribed to; rendering a second, connection-local
// copy of the same events would be a projection only this client can see,
// which is exactly the divergence SR-OBS-003 forbids. An error is the one thing
// the run's published state cannot carry on its own: it names the run as
// failed but not what to tell this caller, which is still waiting on the send
// it made.
type wsRunDelivery struct {
	writer *wsWriter
	ref    wsTurnRef
	// release frees what the handler reserved for this run's session, once
	// the run has ended.
	release func()
}

var _ agentapi.RunDelivery = (*wsRunDelivery)(nil)

func newWSRunDelivery(writer *wsWriter, ref wsTurnRef, release func()) *wsRunDelivery {
	return &wsRunDelivery{writer: writer, ref: ref, release: release}
}

// Deliver implements application.RunDelivery. It reports false once the
// socket is closed: nothing written after that reaches the client.
func (d *wsRunDelivery) Deliver(ctx context.Context, ev agentapi.RunEvent) bool {
	select {
	case <-d.writer.stop:
		return false
	default:
	}
	if ev.Event.Type != native.EventError {
		return true
	}
	// The payload is the event's public form, the one the run published: it
	// names the failure by the code the run records, which the event the
	// runtime raised may not carry yet.
	var public native.StreamEvent
	if err := json.Unmarshal(ev.Payload, &public); err != nil {
		public = ev.Event
	}
	sendWSAgentError(ctx, d.writer, d.ref, public)
	return true
}

// Settle implements application.RunDelivery.
func (*wsRunDelivery) Settle() {}

// Ended implements application.RunDelivery.
func (d *wsRunDelivery) Ended(context.Context, agentapi.RunEnded) {
	if d.release != nil {
		d.release()
	}
}
