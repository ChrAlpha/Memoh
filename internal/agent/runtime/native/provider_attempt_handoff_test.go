package native

import (
	"errors"
	"reflect"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
)

// TestProviderAttemptHandoffRejectRevokesStagedAttempt pins the loop's
// dispatch-boundary reject: a staged attempt that will not be dispatched (dead
// or budget-failed context) leaves the step snapshots, fork snapshot, and
// retry input untouched, revokes the boundary's dynamic
// admission, and clears the stale staging so a later publish cannot reuse it.
func TestProviderAttemptHandoffRejectRevokesStagedAttempt(t *testing.T) {
	t.Parallel()

	oldParams := sdk.Request{Messages: []sdk.Message{sdk.UserMessage("last dispatched")}}
	ledger := contextfrag.NewMutationLedger()
	ledger.AppendStepSnapshot(contextfrag.StepSnapshot{StepIndex: 0})
	fork := agenttools.NewMessageSnapshot(oldParams.Messages)
	attemptState := &providerAttemptState{}
	attemptState.store(&oldParams, 0, false, nil)

	dynamic := &loopDynamicInputs{}
	dynamic.beginBoundary(1)
	injected := sdk.UserMessage("not dispatched")
	newParams := sdk.Request{Messages: []sdk.Message{sdk.UserMessage("next"), injected}}
	dynamic.append(injected, false, "not dispatched", 1)
	refs := dynamic.pendingRefs()
	// The dynamic message was admitted by an earlier publish of the same
	// boundary (the mid-stream retry shape).
	dynamic.commit(refs)
	if got := dynamic.stepAdditions(1); len(got) != 1 {
		t.Fatal("test setup did not admit the pending dynamic message")
	}

	cfg := RunConfig{
		ContextMutations:     ledger,
		ForkContext:          fork,
		providerAttemptState: attemptState,
		dynamicInputs:        dynamic,
	}
	handoff := newProviderAttemptHandoff(cfg)
	handoff.stage(contextfrag.StepSnapshot{StepIndex: 1}, false, refs)

	// The loop's dispatch boundary refuses the attempt instead of publishing.
	handoff.reject()

	if got := ledger.StepSnapshots(); len(got) != 1 || got[0].StepIndex != 0 {
		t.Fatalf("step snapshots = %#v, want only prior dispatched attempt", got)
	}
	if got := ledger.Records(); len(got) != 0 {
		t.Fatalf("mutation records = %#v, want the rejected attempt unpublished", got)
	}
	forkMessages, err := fork.Messages()
	if err != nil {
		t.Fatalf("read fork snapshot: %v", err)
	}
	if !reflect.DeepEqual(forkMessages, oldParams.Messages) {
		t.Fatalf("fork messages = %#v, want prior dispatched messages %#v", forkMessages, oldParams.Messages)
	}
	retryMessages, ok := attemptState.retryMessages(nil)
	if !ok || !reflect.DeepEqual(retryMessages, oldParams.Messages) {
		t.Fatalf("retry messages = %#v, %t; want prior dispatched messages %#v", retryMessages, ok, oldParams.Messages)
	}
	if got := dynamic.stepAdditions(1); len(got) != 0 {
		t.Fatalf("step additions = %#v, want rejected admission revoked", got)
	}
	if err := handoff.publish(newParams); !errors.Is(err, errProviderAttemptNotPrepared) {
		t.Fatalf("publish after reject error = %v, want stale handoff cleared", err)
	}
}

// TestProviderAttemptHandoffPublishAppliesAttemptState pins the publish
// boundary the loops run immediately before invoking the provider: it appends
// the step snapshot, stores the fork and retry
// state, and makes the attempt's dynamic
// messages durable for its step — exactly once per staged attempt.
func TestProviderAttemptHandoffPublishAppliesAttemptState(t *testing.T) {
	t.Parallel()

	dynamic := &loopDynamicInputs{}
	dynamic.beginBoundary(1)
	admitted := sdk.UserMessage("admitted dynamic input")
	params := sdk.Request{
		System:   "system",
		Messages: []sdk.Message{sdk.UserMessage("task"), admitted},
		Tools:    []sdk.ToolDefinition{{Name: "lookup"}},
	}
	dynamic.append(admitted, false, "admitted dynamic input", 1)
	ledger := contextfrag.NewMutationLedger()
	fork := agenttools.NewMessageSnapshot(nil)
	attemptState := &providerAttemptState{}
	cfg := RunConfig{
		ContextMutations:     ledger,
		ForkContext:          fork,
		providerAttemptState: attemptState,
		dynamicInputs:        dynamic,
	}
	handoff := newProviderAttemptHandoff(cfg)
	handoff.stage(contextfrag.StepSnapshot{StepIndex: 1}, false, dynamic.pendingRefs())

	if err := handoff.publish(params); err != nil {
		t.Fatalf("publish provider attempt: %v", err)
	}

	steps := ledger.StepSnapshots()
	if len(steps) != 1 || steps[0].StepIndex != 1 {
		t.Fatalf("step snapshots = %#v, want published step 1", steps)
	}
	forkMessages, err := fork.Messages()
	if err != nil {
		t.Fatalf("read fork snapshot: %v", err)
	}
	if !reflect.DeepEqual(forkMessages, params.Messages) {
		t.Fatalf("fork messages = %#v, want %#v", forkMessages, params.Messages)
	}
	retryMessages, ok := attemptState.retryMessages(nil)
	if !ok || !reflect.DeepEqual(retryMessages, params.Messages) {
		t.Fatalf("retry messages = %#v, %t; want %#v", retryMessages, ok, params.Messages)
	}
	if records := ledger.Records(); len(records) != 0 {
		t.Fatalf("mutation records = %#v, want none", records)
	}
	additions := dynamic.stepAdditions(1)
	if len(additions) != 1 || !reflect.DeepEqual(additions[0], admitted) {
		t.Fatalf("step additions = %#v, want admitted dynamic input", additions)
	}
	if err := handoff.publish(params); !errors.Is(err, errProviderAttemptNotPrepared) {
		t.Fatalf("second publish error = %v, want one-shot staging", err)
	}
}
