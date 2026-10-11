package application

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
)

// decisionOutputSink is the part of the session runtime a decision
// continuation publishes through: the session projection every subscriber
// reads, and the output log of the command that carried the answer.
type decisionOutputSink interface {
	HandleAgentEvent(ctx context.Context, handle sessionruntime.RunHandle, ev native.StreamEvent) ([]chatview.UIMessage, error)
	PublishDecisionOutput(ctx context.Context, command sessionruntime.Command, seq int64, payload json.RawMessage) error
}

// decisionOutputPublisher publishes a decision continuation. Each event goes
// to the session projection and then to the answering command's output log,
// in that order, so a channel reader never sees an event the projection does
// not have. Both writes are the run's publication: either one refused fails
// the run.
//
// The driver calls publish from its consumer and then from its own line once
// the consumer has finished, never concurrently, so seq needs no lock.
type decisionOutputPublisher struct {
	sink    decisionOutputSink
	handle  sessionruntime.RunHandle
	command sessionruntime.Command
	seq     int64
}

func (p *decisionOutputPublisher) publish(ctx context.Context, ev native.StreamEvent, payload json.RawMessage) error {
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
func (p *decisionOutputPublisher) close(ctx context.Context, _ runEnd) error {
	return p.sink.PublishDecisionOutput(ctx, p.command, p.seq+1, nil)
}
