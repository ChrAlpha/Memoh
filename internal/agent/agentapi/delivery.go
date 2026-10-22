package agentapi

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// RunDelivery is the caller's end of a run's events. The driver hands it every
// event it published, in publication order, from one goroutine of its own, and
// never waits for it while the run executes.
type RunDelivery interface {
	// Deliver hands one published event to the caller. It reports false once
	// the caller is gone; the driver then stops delivering and the run goes on.
	Deliver(ctx context.Context, ev RunEvent) bool
	// Settle runs once after the last Deliver and before the run's terminal
	// write.
	Settle()
	// Ended runs once after the terminal write.
	Ended(ctx context.Context, end RunEnded)
}

// RunEvent is one event as it leaves the driver: the public form of the event
// (an error carries its code and args, never its cause) and its JSON, marshalled
// once so every consumer reads the same bytes.
type RunEvent struct {
	Event   native.StreamEvent
	Payload json.RawMessage
}

// RunEnded is how a run ended, as the delivery end sees it.
type RunEnded struct {
	// State is the durable terminal state; empty when the run has none (parked,
	// lost, or a terminal write that failed).
	State     string
	ErrorCode string
	ErrorArgs map[string]string
	// Failure is the public error of the run's failure; nil unless it failed.
	Failure error
}
