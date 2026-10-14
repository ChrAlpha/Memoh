package local

import (
	"testing"
	"time"

	"github.com/felinics/memoh/internal/channel"
)

func TestRouteHubPublishEvent(t *testing.T) {
	t.Parallel()

	hub := NewRouteHub()
	_, stream, cancel := hub.Subscribe("bot-1")
	defer cancel()

	hub.PublishEvent("bot-1", channel.StreamEvent{
		Type:  channel.StreamEventDelta,
		Delta: "hello",
	})

	select {
	case item := <-stream:
		if item.Target != "bot-1" {
			t.Fatalf("unexpected target: %s", item.Target)
		}
		if item.Event.Type != channel.StreamEventDelta {
			t.Fatalf("unexpected event type: %s", item.Event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected event but timed out")
	}
}
