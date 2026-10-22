package drive

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// DecisionOutputSink is the part of the session runtime a decision
// continuation publishes through: the session projection every subscriber
// reads, and the output log of the command that carried the answer.
type DecisionOutputSink interface {
	EventSink
	PublishDecisionOutput(ctx context.Context, command sessionruntime.Command, seq int64, payload json.RawMessage) error
}

// DecisionOutputPublisher publishes a decision continuation. Each event goes
// to the session projection and then to the answering command's output log,
// in that order, so a channel reader never sees an event the projection does
// not have. Both writes are the run's publication: either one refused fails
// the run.
//
// The driver calls publish from its consumer and then from its own line once
// the consumer has finished, never concurrently, so seq needs no lock.
type DecisionOutputPublisher struct {
	sink    DecisionOutputSink
	handle  sessionruntime.RunHandle
	command sessionruntime.Command
	seq     int64
}

// NewDecisionOutputPublisher publishes a decision continuation of the run
// handle through sink, on behalf of command.
func NewDecisionOutputPublisher(sink DecisionOutputSink, handle sessionruntime.RunHandle, command sessionruntime.Command) *DecisionOutputPublisher {
	return &DecisionOutputPublisher{sink: sink, handle: handle, command: command}
}

func (p *DecisionOutputPublisher) publish(ctx context.Context, ev native.StreamEvent, payload json.RawMessage) error {
	if _, err := p.sink.HandleAgentEvent(ctx, p.handle, ev); err != nil {
		return err
	}
	if err := p.sink.PublishDecisionOutput(ctx, p.command, p.seq+1, payload); err != nil {
		return err
	}
	p.seq++
	return nil
}

// close ends the command's output log. A reader waits on that log until it is
// closed, so a log that could not be closed must not leave a parked run
// holding it: the refusal fails the run like any other publication.
func (p *DecisionOutputPublisher) close(ctx context.Context, _ outcome.RunEnd) error {
	return p.sink.PublishDecisionOutput(ctx, p.command, p.seq+1, nil)
}
