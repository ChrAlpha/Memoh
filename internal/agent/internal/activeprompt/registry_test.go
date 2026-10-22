package activeprompt

import (
	"testing"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func TestSubscriberFollowsTheHubUntilItCloses(t *testing.T) {
	t.Parallel()

	registry := New()
	if registry.Has("bot", "session") {
		t.Fatal("an empty registry reports a running prompt")
	}
	if _, ok := registry.Subscribe("bot", "session"); ok {
		t.Fatal("subscribed to a prompt that is not running")
	}
	hub := registry.Register(" bot ", "session")
	if !registry.Has("bot", " session ") {
		t.Fatal("the registered prompt is not reported")
	}
	sub, ok := registry.Subscribe("bot", "session")
	if !ok {
		t.Fatal("no subscription to a running prompt")
	}
	hub.Emit(native.StreamEvent{Type: native.EventStart})
	ev, open, err := sub.Next(t.Context())
	if err != nil || !open || ev.Type != native.EventStart {
		t.Fatalf("Next() = (%v, %t, %v), want the emitted event", ev.Type, open, err)
	}

	registry.Unregister("bot", "session", hub)
	if registry.Has("bot", "session") {
		t.Fatal("the unregistered prompt is still reported")
	}
	if _, open, err := sub.Next(t.Context()); err != nil || open {
		t.Fatalf("Next() after the hub closed = (%t, %v), want a closed stream", open, err)
	}
	sub.Release()
}

func TestRegisterClosesTheHubItReplaces(t *testing.T) {
	t.Parallel()

	registry := New()
	first := registry.Register("bot", "session")
	sub, ok := registry.Subscribe("bot", "session")
	if !ok {
		t.Fatal("no subscription to a running prompt")
	}
	second := registry.Register("bot", "session")
	if _, open, err := sub.Next(t.Context()); err != nil || open {
		t.Fatalf("Next() after replacement = (%t, %v), want the replaced hub closed", open, err)
	}
	// The stale owner unregistering must not drop its successor.
	registry.Unregister("bot", "session", first)
	if !registry.Has("bot", "session") {
		t.Fatal("unregistering a replaced hub dropped the current one")
	}
	registry.Unregister("bot", "session", second)
}
