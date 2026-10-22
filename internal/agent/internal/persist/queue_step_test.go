package persist

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// A deferred step parks the loop, so its commit must not claim a steer: a
// claim would sit unapplied across the decision and across any owner change
// while the run waits. The continuation's first committed step claims it
// instead, and the next step applies it once.
func TestDeferredStepDoesNotClaimSteerAndContinuationDeliversIt(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := context.Background()
	key := sessionruntime.Key{BotID: handle.BotID, SessionID: handle.SessionID}
	item, err := fx.enqueueSteer(t, "invoke-1", "steer me")
	if err != nil {
		t.Fatal(err)
	}

	// Original run: the deferred step commits without touching the queue.
	original := newQueueStepCoordinator(fx.writer, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID,
		RunHandle: handle, QueueSteerEnabled: true,
	})
	if original == nil {
		t.Fatal("queue step transaction unavailable")
	}
	outcome, err := original.commit(ctx, queueStepDeferredDecision, messagepkg.AgentStep{RunID: handle.RunID})
	if err != nil {
		t.Fatalf("deferred commit: %v", err)
	}
	if outcome.claimedSteer != nil || outcome.appliedSteerItemID != "" {
		t.Fatalf("deferred step touched the steer queue: %#v", outcome)
	}

	if steers, err := fx.manager.PendingSteers(ctx, key, 0); err != nil || len(steers) != 1 || steers[0].Status != sessionruntime.QueueAccepted {
		t.Fatalf("steer should stay accepted across the park: %#v, %v", steers, err)
	}

	// Continuation after the decision uses a fresh coordinator with the same run.
	continuation := newQueueStepCoordinator(fx.writer, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID,
		RunHandle: handle, QueueSteerEnabled: true, UserMessagePersisted: true,
	})
	if continuation == nil {
		t.Fatal("continuation queue step transaction unavailable")
	}

	// Step N+1: the model call that consumed the approved tool result. Its
	// commit claims the steer for step N+2.
	outcome, err = continuation.commit(ctx, queueStepToolLoop, messagepkg.AgentStep{RunID: handle.RunID})
	if err != nil {
		t.Fatalf("continuation commit: %v", err)
	}
	if outcome.appliedSteerItemID != "" {
		t.Fatalf("continuation applied a steer it never delivered: %#v", outcome)
	}
	if outcome.claimedSteer == nil || outcome.claimedSteer.ID != item.ID {
		t.Fatalf("continuation did not claim the steer: %#v", outcome)
	}
	if got := turnwire.ParseQueuedInput(outcome.claimedSteer.Payload).VisibleText(); got != "steer me" {
		t.Fatalf("claimed payload = %q", got)
	}

	steers, err := fx.manager.PendingSteers(ctx, key, 0)
	if err != nil || len(steers) != 0 {
		t.Fatalf("pending steers while claimed = %#v, %v", steers, err)
	}

	// Step N+2 saw the steer; its commit applies the claim exactly once.
	outcome, err = continuation.commit(ctx, queueStepFinal, messagepkg.AgentStep{RunID: handle.RunID})
	if err != nil {
		t.Fatalf("final commit: %v", err)
	}
	if outcome.appliedSteerItemID != string(item.ID) || outcome.claimedSteer != nil {
		t.Fatalf("final outcome = %#v", outcome)
	}
	if _, err := fx.manager.UpdateSteer(ctx, key, item.ID, []byte("x")); err == nil {
		t.Fatal("applied steer still mutable")
	}
	if _, err := fx.enqueueSteer(t, "invoke-2", "late"); !errors.Is(err, sessionruntime.ErrQueueNoActiveRun) {
		t.Fatalf("late steer after sealed final = %v", err)
	}
}

func TestFinalStepClaimsSteerForLoopContinuation(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := context.Background()
	item, err := fx.enqueueSteer(t, "invoke-1", "change direction")
	if err != nil {
		t.Fatal(err)
	}

	coordinator := newQueueStepCoordinator(fx.writer, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID,
		RunHandle: handle, QueueSteerEnabled: true,
	})
	if coordinator == nil {
		t.Fatal("queue step coordinator unavailable")
	}
	outcome, err := coordinator.commit(ctx, queueStepFinal, messagepkg.AgentStep{RunID: handle.RunID})
	if err != nil {
		t.Fatalf("final commit: %v", err)
	}
	if outcome.claimedSteer == nil || outcome.claimedSteer.ID != item.ID {
		t.Fatalf("final did not claim the steer: %#v", outcome)
	}
	if got := turnwire.ParseQueuedInput(outcome.claimedSteer.Payload).VisibleText(); got != "change direction" {
		t.Fatalf("claimed payload = %q", got)
	}
	if _, err := fx.enqueueSteer(t, "invoke-2", "late"); err != nil {
		t.Fatalf("run should stay open after a claimed final steer: %v", err)
	}
}

// The step that applies a steer records, in the same commit, that the queued
// input it was promoted from is taken. The terminal pass would find it too, but
// only after the run is over.
func TestStepThatAppliesASteerRecordsThePromotedInputAsTaken(t *testing.T) {
	fx := newSteerFixture(t)
	handle := fx.handle
	ctx := context.Background()
	item, err := fx.enqueueSteer(t, "invoke-1", "steer me")
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newQueueStepCoordinator(fx.writer, runreq.ChatRequest{
		BotID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID,
		RunHandle: handle, QueueSteerEnabled: true,
	})
	if coordinator == nil {
		t.Fatal("queue step coordinator unavailable")
	}

	if _, err := coordinator.commit(ctx, queueStepFinal, messagepkg.AgentStep{RunID: handle.RunID}); err != nil {
		t.Fatalf("claiming commit: %v", err)
	}
	if got := fx.promoted.consumedIDs(); len(got) != 0 {
		t.Fatalf("a claimed steer is not yet applied, but %v were recorded", got)
	}
	if _, err := coordinator.commit(ctx, queueStepFinal, messagepkg.AgentStep{RunID: handle.RunID}); err != nil {
		t.Fatalf("applying commit: %v", err)
	}
	if got := fx.promoted.consumedIDs(); len(got) != 1 || got[0] != string(item.ID) {
		t.Fatalf("recorded as taken = %v, want [%s]", got, item.ID)
	}
}
