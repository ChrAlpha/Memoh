package local

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/channel"
)

// RouteHubEvent is a routed outbound stream event for local transports.
type RouteHubEvent struct {
	Target string              `json:"target"`
	Event  channel.StreamEvent `json:"event"`
}

// RouteHub is a pub/sub hub that routes outbound messages to local subscribers by route key.
type RouteHub struct {
	mu      sync.RWMutex
	streams map[string]map[string]chan RouteHubEvent
}

// NewRouteHub creates an empty RouteHub.
func NewRouteHub() *RouteHub {
	return &RouteHub{
		streams: map[string]map[string]chan RouteHubEvent{},
	}
}

// Subscribe registers a new stream for the given route key and returns a stream ID,
// a read-only channel for messages, and a cancel function to unsubscribe.
func (h *RouteHub) Subscribe(routeKey string) (string, <-chan RouteHubEvent, func()) {
	streamID := uuid.NewString()
	ch := make(chan RouteHubEvent, 32)

	h.mu.Lock()
	streams, ok := h.streams[routeKey]
	if !ok {
		streams = map[string]chan RouteHubEvent{}
		h.streams[routeKey] = streams
	}
	streams[streamID] = ch
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		streams := h.streams[routeKey]
		if streams != nil {
			if current, ok := streams[streamID]; ok {
				delete(streams, streamID)
				close(current)
			}
			if len(streams) == 0 {
				delete(h.streams, routeKey)
			}
		}
		h.mu.Unlock()
	}

	return streamID, ch, cancel
}

// Publish delivers a message to all subscribers of the given route key.
// Slow receivers are silently dropped.
func (h *RouteHub) Publish(routeKey string, msg channel.OutboundMessage) {
	h.PublishEvent(routeKey, channel.StreamEvent{
		Type: channel.StreamEventFinal,
		Final: &channel.StreamFinalizePayload{
			Message: msg.Message,
		},
	})
}

// PublishEvent delivers a stream event to all subscribers of the given route key.
// Slow receivers are silently dropped.
func (h *RouteHub) PublishEvent(routeKey string, event channel.StreamEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.streams[routeKey] {
		payload := RouteHubEvent{
			Target: routeKey,
			Event:  event,
		}
		select {
		case ch <- payload:
		default:
			// Drop if receiver is slow.
		}
	}
}

// localPreview shows one reply by forwarding each of its events to the
// subscribers of the route as it happens. The route hub is the stream the
// local channel natively shows replies on, so nothing is held back or edited.
type localPreview struct {
	hub    *RouteHub
	target string
	ended  atomic.Bool
}

var _ channel.PreparedPreview = (*localPreview)(nil)

func newLocalPreview(hub *RouteHub, target string) *localPreview {
	return &localPreview{
		hub:    hub,
		target: target,
	}
}

func (p *localPreview) Update(ctx context.Context, event channel.PreparedStreamEvent) error {
	if p.ended.Load() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.hub.PublishEvent(p.target, event.LogicalEvent())
	return nil
}

// Finish publishes the final, if there is one, and then the completed status
// that closes the reply for subscribers.
func (p *localPreview) Finish(ctx context.Context, final *channel.PreparedStreamEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.ended.CompareAndSwap(false, true) {
		return nil
	}
	if final != nil {
		p.hub.PublishEvent(p.target, final.LogicalEvent())
	}
	p.hub.PublishEvent(p.target, channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusCompleted})
	return nil
}

// Abort ends the reply without the completed status.
func (p *localPreview) Abort(context.Context) error {
	p.ended.Store(true)
	return nil
}
