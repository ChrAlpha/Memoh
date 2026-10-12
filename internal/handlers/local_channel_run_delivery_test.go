package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/application"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func deliveredRunEvent(t *testing.T, event native.StreamEvent) application.RunEvent {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return application.RunEvent{Event: event, Payload: payload}
}

// The initiating socket reads the run through its session subscription like
// every other subscriber. Its delivery end writes a frame for an error alone:
// rendering anything else would be a second, connection-local projection.
func TestWSRunDeliverySendsOnlyErrors(t *testing.T) {
	t.Parallel()

	writer := &wsWriter{ch: make(chan []byte, 16), stop: make(chan struct{}), done: make(chan struct{})}
	ref := wsTurnRef{RunID: "run-1", SessionID: "session-1"}
	delivery := newWSRunDelivery(writer, ref, nil)
	for _, event := range []native.StreamEvent{
		{Type: native.EventAgentStart},
		{Type: native.EventTextDelta, Delta: "hello"},
		{Type: native.EventToolCallStart, ToolName: "exec", ToolCallID: "call-1"},
		{Type: native.EventAttachment},
		{Type: native.EventError, Code: "agent.provider_overloaded", Error: "agent.provider_overloaded"},
		{Type: native.EventAgentAbort, Code: "agent.provider_overloaded"},
	} {
		if !delivery.Deliver(context.Background(), deliveredRunEvent(t, event)) {
			t.Fatalf("Deliver(%s) reported the socket gone", event.Type)
		}
	}

	if len(writer.ch) != 1 {
		t.Fatalf("frames written = %d, want the error alone", len(writer.ch))
	}
	var frame map[string]any
	if err := json.Unmarshal(<-writer.ch, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame["type"] != "error" || frame["code"] != "agent.provider_overloaded" || frame["run_id"] != "run-1" {
		t.Fatalf("frame = %#v, want the run's error by code", frame)
	}
}

// The frame names the failure the run published, which the event the runtime
// raised does not always carry: the run names an uncoded failure from its
// cause before publishing it.
func TestWSRunDeliveryNamesTheFailureThePayloadPublished(t *testing.T) {
	t.Parallel()

	writer := &wsWriter{ch: make(chan []byte, 4), stop: make(chan struct{}), done: make(chan struct{})}
	delivery := newWSRunDelivery(writer, wsTurnRef{RunID: "run-1"}, nil)
	public, err := json.Marshal(native.StreamEvent{Type: native.EventError, Code: "agent.provider_rate_limited", Error: "agent.provider_rate_limited"})
	if err != nil {
		t.Fatal(err)
	}
	delivery.Deliver(context.Background(), application.RunEvent{
		Event:   native.StreamEvent{Type: native.EventError, Error: "429 from upstream"},
		Payload: public,
	})

	var frame map[string]any
	if err := json.Unmarshal(<-writer.ch, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame["code"] != "agent.provider_rate_limited" {
		t.Fatalf("frame code = %#v, want the published code", frame["code"])
	}
	if text, _ := json.Marshal(frame); strings.Contains(string(text), "429 from upstream") {
		t.Fatalf("frame %s carries the runtime's own text", text)
	}
}

// A closed socket is a caller that has gone: the run stops delivering to it and
// goes on.
func TestWSRunDeliveryReportsAClosedSocket(t *testing.T) {
	t.Parallel()

	writer := &wsWriter{ch: make(chan []byte, 1), stop: make(chan struct{}), done: make(chan struct{})}
	close(writer.stop)
	delivery := newWSRunDelivery(writer, wsTurnRef{RunID: "run-1"}, nil)
	if delivery.Deliver(context.Background(), deliveredRunEvent(t, native.StreamEvent{Type: native.EventError, Code: "runtime_run_failed"})) {
		t.Fatal("Deliver to a closed socket reported the caller still there")
	}
}

// What the handler reserved for the run's session is released once the run
// has ended, and not before.
func TestWSRunDeliveryReleasesWhenTheRunEnds(t *testing.T) {
	t.Parallel()

	writer := &wsWriter{ch: make(chan []byte, 1), stop: make(chan struct{}), done: make(chan struct{})}
	released := 0
	delivery := newWSRunDelivery(writer, wsTurnRef{RunID: "run-1"}, func() { released++ })
	delivery.Deliver(context.Background(), deliveredRunEvent(t, native.StreamEvent{Type: native.EventTextDelta, Delta: "a"}))
	delivery.Settle()
	if released != 0 {
		t.Fatalf("released %d times before the run ended", released)
	}
	delivery.Ended(context.Background(), application.RunEnded{State: "completed"})
	if released != 1 {
		t.Fatalf("released %d times, want once when the run ended", released)
	}
}
