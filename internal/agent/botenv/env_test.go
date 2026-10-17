package botenv

import (
	"context"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/hooks"
)

func TestNewDefaultsClockLocationToUTC(t *testing.T) {
	t.Parallel()

	name, location := New(Deps{}).ResolveTimezone(context.Background(), "", "")
	if location != time.UTC || name != "UTC" {
		t.Fatalf("timezone = (%q, %v), want UTC", name, location)
	}
	zone := time.FixedZone("Test/Zone", 3600)
	if _, got := New(Deps{ClockLocation: zone}).ResolveTimezone(context.Background(), "", ""); got != zone {
		t.Fatalf("location = %v, want the configured default", got)
	}
}

func TestWithoutHooksEveryEventIsAllowed(t *testing.T) {
	t.Parallel()

	called := false
	result, err := New(Deps{}).RunChatHook(context.Background(), runreq.ChatRequest{}, hooks.EventUserMessageReceived, func(*hooks.Request) { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != hooks.DecisionAllow || result.RuntimeSupported != hooks.RuntimeSupported(hooks.EventUserMessageReceived) {
		t.Fatalf("result = %#v, want an allow carrying the event's runtime support", result)
	}
	if called {
		t.Fatal("the request was built for an event nothing listens to")
	}
}
