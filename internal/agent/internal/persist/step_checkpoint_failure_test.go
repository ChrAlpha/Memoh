package persist

import (
	"context"
	"errors"
	"slices"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/step"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func checkpointCommitter(owner context.Context, store *agenttest.RecordingStepPersister) *StepCommitter {
	return &StepCommitter{
		onPersisted: func() {},
		writer:      newTestWriter(Deps{}), persister: store, ownerContext: owner,
		req: runreq.ChatRequest{BotID: "bot", ThreadID: "session", RunID: "run", UserMessagePersisted: true},
		rc:  runreq.ResolvedContext{RunConfig: native.RunConfig{ContextLifecycle: contextfrag.NewLifecycleHolder()}},
	}
}

func writeCheckpoint(t *testing.T, committer *StepCommitter, cause error) {
	t.Helper()
	cancelled, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	if err := committer.interrupt(cancelled, 0, &step.Record{Messages: []sdk.Message{sdk.AssistantMessage("partial")}}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
}

// The checkpoint is written while the stream unwinds, before the driver names
// the run's failure. It carries no code of its own: the failure the driver
// names, which may differ from the cancellation's cause, is written onto it.
func TestCheckpointRowTakesTheFailureTheRunWasNamedWith(t *testing.T) {
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	committer := checkpointCommitter(context.Background(), store)
	writeCheckpoint(t, committer, context.DeadlineExceeded)

	if len(store.Steps) != 1 {
		t.Fatalf("checkpoint writes = %d, want 1", len(store.Steps))
	}
	if _, coded := store.Steps[0].Messages[0].Metadata[messagepkg.HistoryErrorCodeMetadataKey]; coded {
		t.Fatalf("checkpoint named a failure of its own: %#v", store.Steps[0].Messages[0].Metadata)
	}

	declared := outcome.RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: outcome.PhaseExecution}
	if _, err := committer.RecordFailure(context.Background(), "partial", declared); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if len(store.Steps) != 1 {
		t.Fatalf("a second row was written: %d steps", len(store.Steps))
	}
	if len(store.InterruptedFailures) != 1 {
		t.Fatalf("named failures = %d, want 1", len(store.InterruptedFailures))
	}
	got := store.InterruptedFailures[0]
	if got.RunID != "run" || got.BotID != "bot" || got.SessionID != "session" ||
		got.Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(declared.Code) {
		t.Fatalf("named failure = %#v, want the declared code %s", got, declared.Code)
	}
	if want := []string{"committed"}; !slices.Equal(got.MessageIDs, want) {
		t.Fatalf("named rows = %v, want the checkpoint the run wrote %v", got.MessageIDs, want)
	}
	rows := committer.PersistedMessages()
	if len(rows) != 1 || rows[0].Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(declared.Code) ||
		rows[0].Metadata[messagepkg.AgentStepInterruptedMetadataKey] != true {
		t.Fatalf("reported rows = %#v", rows)
	}
}

// A stop names no failure, so nothing is written to the checkpoint.
func TestStoppedRunLeavesTheCheckpointWithoutCode(t *testing.T) {
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	committer := checkpointCommitter(context.Background(), store)
	writeCheckpoint(t, committer, context.Canceled)
	if _, err := committer.RecordFailure(context.Background(), "partial", outcome.RunFailure{}); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if len(store.Steps) != 1 || len(store.InterruptedFailures) != 0 {
		t.Fatalf("steps=%d named=%d, want 1 and 0", len(store.Steps), len(store.InterruptedFailures))
	}
	if _, coded := store.Steps[0].Messages[0].Metadata[messagepkg.HistoryErrorCodeMetadataKey]; coded {
		t.Fatalf("stopped checkpoint carries a code: %#v", store.Steps[0].Messages[0].Metadata)
	}
}

// Without a checkpoint the failure is a trailing row, and nothing is merged.
func TestFailureWithoutCheckpointWritesTheTrailingRow(t *testing.T) {
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	committer := checkpointCommitter(context.Background(), store)
	failure := outcome.RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: outcome.PhaseExecution}
	if _, err := committer.RecordFailure(context.Background(), "partial", failure); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if len(store.Steps) != 1 || len(store.InterruptedFailures) != 0 {
		t.Fatalf("steps=%d named=%d, want 1 and 0", len(store.Steps), len(store.InterruptedFailures))
	}
	if store.Steps[0].Messages[0].Metadata[messagepkg.HistoryErrorCodeMetadataKey] != string(failure.Code) {
		t.Fatalf("trailing row metadata = %#v", store.Steps[0].Messages[0].Metadata)
	}
}

// A refused update is an error of the ending, and a revoked owner never writes.
func TestNamingTheCheckpointFailsWhenTheWriteIsRefused(t *testing.T) {
	failure := outcome.RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: outcome.PhaseExecution}

	refused := errors.New("not writable")
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{InterruptedFailureErr: refused}}
	committer := checkpointCommitter(context.Background(), store)
	writeCheckpoint(t, committer, context.DeadlineExceeded)
	if _, err := committer.RecordFailure(context.Background(), "", failure); !errors.Is(err, refused) {
		t.Fatalf("refused update error = %v", err)
	}

	store = &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	owner, cancel := context.WithCancelCause(context.Background())
	committer = checkpointCommitter(owner, store)
	writeCheckpoint(t, committer, context.DeadlineExceeded)
	cancel(sessionruntime.ErrRunOwnershipLost)
	if _, err := committer.RecordFailure(context.Background(), "", failure); !errors.Is(err, sessionruntime.ErrRunOwnershipLost) {
		t.Fatalf("revoked owner error = %v", err)
	}
	if len(store.InterruptedFailures) != 0 {
		t.Fatalf("a revoked owner named a failure: %#v", store.InterruptedFailures)
	}
}

// A run that wrote no checkpoint has nothing to name: NameCheckpoint writes no
// row, which is what lets a run without a failure row of its own call it.
func TestNameCheckpointWithoutCheckpointWritesNothing(t *testing.T) {
	store := &agenttest.RecordingStepPersister{RecordingMessages: &agenttest.RecordingMessages{}}
	committer := checkpointCommitter(context.Background(), store)
	failure := outcome.RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Phase: outcome.PhaseExecution}
	if err := committer.NameCheckpoint(context.Background(), failure); err != nil {
		t.Fatalf("NameCheckpoint: %v", err)
	}
	if len(store.Steps) != 0 {
		t.Fatalf("a row was written: %#v", store.Steps)
	}
}
