package runreq

import (
	"context"
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// SendAgentStreamEvent delivers ev on eventCh unless ctx ends first.
func SendAgentStreamEvent(ctx context.Context, eventCh chan<- WSStreamEvent, ev native.StreamEvent) error {
	if eventCh == nil {
		return nil
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	select {
	case eventCh <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WSStreamEvent represents a raw JSON event forwarded from the agent.
type WSStreamEvent = json.RawMessage

// EmitApprovalAck sends an agent start and end pair so the stream sees the
// approval response as handled. A nil channel is a no-op.
func EmitApprovalAck(ctx context.Context, eventCh chan<- WSStreamEvent) error {
	if eventCh == nil {
		return nil
	}
	for _, event := range []native.StreamEvent{
		{Type: native.EventAgentStart},
		{Type: native.EventAgentEnd},
	} {
		if err := SendAgentStreamEvent(ctx, eventCh, event); err != nil {
			return err
		}
	}
	return nil
}
