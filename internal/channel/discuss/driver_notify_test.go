package discuss

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/chat/timeline"
)

type fakeEventLoader struct {
	mu      sync.Mutex
	events  []timeline.CanonicalEvent
	calls   int
	entered chan struct{}
	gate    chan struct{}
}

func (f *fakeEventLoader) LoadEventsForReplay(_ context.Context, _, _ string) ([]timeline.CanonicalEvent, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	events := f.events
	f.mu.Unlock()
	if n == 1 && f.entered != nil {
		close(f.entered)
		<-f.gate
	}
	return events, nil
}

func (f *fakeEventLoader) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNotifyWakesWorkerToReadCommittedEvents(t *testing.T) {
	loader := &fakeEventLoader{events: []timeline.CanonicalEvent{
		timeline.MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1, Content: []timeline.ContentNode{{Type: "text", Text: "hello committed"}}},
	}}
	driver := NewDiscussDriver(DiscussDriverDeps{Events: loader})
	defer driver.StopAll()

	driver.Notify("s", DiscussSessionConfig{BotID: "bot-1", ThreadID: "s"})
	waitFor(t, func() bool { return loader.callCount() >= 1 })

	rc, err := driver.loadRC(context.Background(), "bot-1", "s")
	if err != nil {
		t.Fatal(err)
	}
	if got := timeline.RCToXML(rc); !strings.Contains(got, "hello committed") {
		t.Fatalf("rendered context lacks the committed message: %s", got)
	}
}

func TestNotifyBurstCoalescesIntoOneRead(t *testing.T) {
	loader := &fakeEventLoader{entered: make(chan struct{}), gate: make(chan struct{})}
	driver := NewDiscussDriver(DiscussDriverDeps{Events: loader})
	defer driver.StopAll()
	cfg := DiscussSessionConfig{BotID: "bot-1", ThreadID: "s"}

	driver.Notify("s", cfg)
	<-loader.entered
	// The worker is mid-read: signals sent now are for events the read may
	// not have seen, so exactly one more read must follow, not one per signal.
	for range 10 {
		driver.Notify("s", cfg)
	}
	close(loader.gate)

	waitFor(t, func() bool { return loader.callCount() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if got := loader.callCount(); got != 2 {
		t.Fatalf("loads = %d, want 2 (in-flight read plus one coalesced read)", got)
	}
}
