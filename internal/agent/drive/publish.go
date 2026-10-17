package drive

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// RunPublisher writes a run's live state. A refusal is a run failure
// (outcome.PhasePublish) unless it says the run is no longer this process's.
// The interface is sealed: the projection publisher and DecisionOutputPublisher
// are the implementations.
type RunPublisher interface {
	publish(ctx context.Context, ev native.StreamEvent, payload json.RawMessage) error
	// close ends whatever the publisher keeps open for the run once its last
	// event is published. A publisher that keeps nothing open does nothing.
	close(ctx context.Context, end outcome.RunEnd) error
}

// projectionPublisher writes the run's events into the session runtime's live
// projection, which is what every subscriber reads.
type projectionPublisher struct {
	handle sessionruntime.RunHandle
	events EventSink
}

func (p projectionPublisher) publish(ctx context.Context, ev native.StreamEvent, _ json.RawMessage) error {
	_, err := p.events.HandleAgentEvent(ctx, p.handle, ev)
	return err
}

func (projectionPublisher) close(context.Context, outcome.RunEnd) error { return nil }

// publisherFor picks the publisher for a run: the entry's own when it named
// one, the session projection otherwise, and none for a run nobody admitted.
func (d *Driver) publisherFor(spec RunSpec) RunPublisher {
	if spec.Run.Handle.FencingToken <= 0 {
		return nil
	}
	if spec.Publisher != nil {
		return spec.Publisher
	}
	return projectionPublisher{handle: spec.Run.Handle, events: d.events}
}

// publisherRefused is the publisher a run uses after a refusal: it accepts
// nothing, so a run whose publisher already failed does not fail again on its
// terminal event. Subscribers converge on the run's terminal write instead.
type publisherRefused struct{}

func (publisherRefused) publish(context.Context, native.StreamEvent, json.RawMessage) error {
	return nil
}

func (publisherRefused) close(context.Context, outcome.RunEnd) error { return nil }

// publishRefused is the cause a run is cancelled with when publishing its
// events was refused. It is not a stop: nobody asked this run to end, and the
// refusal keeps its own cause so the failure policy names it
// (session_runtime.publish_failed when nothing catalogued it).
type PublishRefused struct{ error }

func (e PublishRefused) Unwrap() error { return e.error }
