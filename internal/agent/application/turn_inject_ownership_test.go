package application

import (
	"context"
	"sync"
	"testing"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// A run stops taking direct injections as soon as its execution ends, but its
// terminal write still runs afterwards: the session runtime is still appending
// to the run it is closing, and releasing the thread's slot in the same breath
// as the last event would let the next run overlap it.
//
// The driver's order is fixed: delivery settles, the terminal write runs, and
// only then does the run end for its caller.
func TestRunEndStopsDirectInjectBeforeSessionFinishes(t *testing.T) {
	fixture := newChatTurnFixture(t, directLifecycleModelSuccess)
	fixture.admitter.finishStarted = make(chan struct{})
	fixture.admitter.finishRelease = make(chan struct{})
	var releaseOnce sync.Once
	releaseFinish := func() {
		releaseOnce.Do(func() { close(fixture.admitter.finishRelease) })
	}
	t.Cleanup(releaseFinish)

	handle := fixture.start(t, turn.StartTurnCommand{TeamID: "team-1", IdempotencyKey: "inject-ownership-1"})
	select {
	case <-fixture.admitter.finishStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("durable finisher did not start")
	}
	// The run's execution is over, so a new steer is refused rather than left
	// to rot in a channel nobody will read.
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "late"}); err == nil {
		t.Error("direct inject succeeded after execution stopped")
	}
	// The run has not ended: its terminal write is still in flight, so its
	// caller must not see the run close yet.
	select {
	case _, ok := <-handle.Events():
		if !ok {
			t.Fatal("run ended for its caller before the terminal write finished")
		}
	default:
	}
	releaseFinish()
	collectEvents(t, handle)
	if got := fixture.admitter.awaitFinish(t).status; got != sessionruntime.RunStatusCompleted {
		t.Fatalf("finish status = %q, want completed", got)
	}
	if err := handle.Inject(context.Background(), turn.InjectMessage{Text: "too late"}); err == nil {
		t.Error("direct inject succeeded after the run ended")
	}
}
