package agenttest

import (
	"context"
	"sync"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
)

// EventRecorder stands in for the session-runtime projection
// sink: it records every published event and can be armed to refuse the Nth
// publish, which is how fence rejections surface to the consumer.
type EventRecorder struct {
	mu        sync.Mutex
	events    []native.StreamEvent
	Calls     int
	ErrAt     int // 1-based call index to fail; 0 means never fail
	Err       error
	OnPublish func(native.StreamEvent)
}

func (p *EventRecorder) HandleAgentEvent(_ context.Context, _ sessionruntime.RunHandle, event native.StreamEvent) ([]chatview.UIMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls++
	if p.ErrAt > 0 && p.Calls == p.ErrAt {
		return nil, p.Err
	}
	if p.OnPublish != nil {
		p.OnPublish(event)
	}
	p.events = append(p.events, event)
	return nil, nil
}

// Published is every event recorded so far, in order.
func (p *EventRecorder) Published() []native.StreamEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]native.StreamEvent(nil), p.events...)
}

// EventFunc adapts a function to the session runtime's event intake.
type EventFunc func(context.Context, sessionruntime.RunHandle, native.StreamEvent) error

// HandleAgentEvent calls the function.
func (f EventFunc) HandleAgentEvent(ctx context.Context, handle sessionruntime.RunHandle, event native.StreamEvent) ([]chatview.UIMessage, error) {
	return nil, f(ctx, handle, event)
}
