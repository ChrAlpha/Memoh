package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

func startDirectLifecycleTurn(t *testing.T, fixture directLifecycleFixture) {
	t.Helper()
	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                agenttest.LifecycleBotID,
		ChatID:               agenttest.LifecycleBotID,
		ThreadID:             agenttest.LifecycleSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	for range handle.Events() {
	}
	for range handle.Errs() {
	}
}

func TestAdmittedStreamPersistsCompletedLifecycleAfterAssistant(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess)

	startDirectLifecycleTurn(t, fixture)

	if len(fixture.messages.Persisted) == 0 {
		t.Fatal("the run did not persist its assistant response")
	}
	runID := fixture.messages.Persisted[len(fixture.messages.Persisted)-1].RunID
	if runID != agenttest.LifecycleRunID {
		t.Fatalf("persisted run ID = %q, want the admitted run %q", runID, agenttest.LifecycleRunID)
	}
	assertDirectLifecycle(t, fixture.lifecycles, runID, lifecycle.StatusCompleted, "message-id")
}

func TestAdmittedStreamProviderFailurePersistsFailedProviderLifecycle(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelFailure)

	startDirectLifecycleTurn(t, fixture)

	creates := fixture.lifecycles.creates()
	if len(creates) != 1 {
		t.Fatalf("lifecycle creates = %d, want 1", len(creates))
	}
	assertDirectLifecycle(t, fixture.lifecycles, agenttest.LifecycleRunID, lifecycle.StatusFailedProvider, "message-id")
}

func TestAdmittedStreamCancellationPersistsAbortedLifecycle(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelBlock)

	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                agenttest.LifecycleBotID,
		ChatID:               agenttest.LifecycleBotID,
		ThreadID:             agenttest.LifecycleSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming provider did not start")
	}
	handle.Cancel()
	done := make(chan struct{})
	go func() {
		for range handle.Events() {
		}
		for range handle.Errs() {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled admitted stream did not finish")
	}

	assertDirectLifecycle(t, fixture.lifecycles, agenttest.LifecycleRunID, lifecycle.StatusAborted, "")
	if len(fixture.runtime.Finishes) != 1 || fixture.runtime.Finishes[0].Status != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", fixture.runtime.Finishes)
	}
}

func TestAdmittedStreamWithholdsTerminalWhenHistoryPersistenceFails(t *testing.T) {
	persistErr := errors.New("history unavailable")

	var publishedMu sync.Mutex
	var published []native.StreamEvent
	fixture := newDirectLifecycleFixture(t, directLifecycleModelSuccess, withFixtureOptions(withDrive(func(d *drive.Deps) {
		d.Events = agenttest.EventFunc(func(_ context.Context, _ sessionruntime.RunHandle, event native.StreamEvent) error {
			publishedMu.Lock()
			defer publishedMu.Unlock()
			published = append(published, event)
			return nil
		})
	})))
	fixture.messages.RoundPersistErr = persistErr

	handle, err := fixture.service.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                agenttest.LifecycleBotID,
		ChatID:               agenttest.LifecycleBotID,
		ThreadID:             agenttest.LifecycleSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	for range handle.Events() {
	}
	for range handle.Errs() {
	}

	if len(fixture.runtime.Finishes) != 1 || fixture.runtime.Finishes[0].Status != sessionruntime.RunStatusErrored {
		t.Fatalf("runtime finishes = %#v, want one errored finish", fixture.runtime.Finishes)
	}
	publishedMu.Lock()
	defer publishedMu.Unlock()
	for _, event := range published {
		if event.IsTerminal() {
			t.Fatalf("terminal event %q was published after failed history persistence", event.Type)
		}
	}
}

// gap 2 regression: a caller that goes away while its run is streaming named
// the ending a failure before. The caller's context ending is this run's stop
// signal, so the run must end stopped — not errored with a code nobody raised.
func TestAdmittedStreamCallerContextEndingIsAStop(t *testing.T) {
	fixture := newDirectLifecycleFixture(t, directLifecycleModelBlock)

	caller, stopCaller := context.WithCancel(context.Background())
	handle, err := fixture.service.StartTurn(caller, turn.StartTurnCommand{
		SchemaVersion:        1,
		TeamID:               "direct-lifecycle-team",
		Mode:                 turn.ModeChat,
		BotID:                agenttest.LifecycleBotID,
		ChatID:               agenttest.LifecycleBotID,
		ThreadID:             agenttest.LifecycleSessionID,
		Query:                directLifecyclePrompt,
		UserMessagePersisted: true,
	})
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	select {
	case <-fixture.started:
	case <-time.After(3 * time.Second):
		t.Fatal("streaming provider did not start")
	}

	// The caller's own context goes away, as it does when an inbound consumer
	// disconnects. It never touches this run's stop marker directly.
	stopCaller()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range handle.Events() {
		}
		for range handle.Errs() {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish after its caller went away")
	}

	if len(fixture.runtime.Finishes) != 1 || fixture.runtime.Finishes[0].Status != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", fixture.runtime.Finishes)
	}
	if fixture.runtime.Finishes[0].Message != "" {
		t.Fatalf("finish message = %q, want none for a stop", fixture.runtime.Finishes[0].Message)
	}
	assertDirectLifecycle(t, fixture.lifecycles, agenttest.LifecycleRunID, lifecycle.StatusAborted, "")
}
