package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/answer"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/drivetest"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	sessiontest "github.com/felinics/memoh/internal/testutil/sessionruntime"
)

type decisionOutputBackend struct {
	*sessionruntime.MemoryBackend
	output []struct {
		Type   string
		Output json.RawMessage
	}
}

func (b *decisionOutputBackend) AppendDecisionOutput(ctx context.Context, ref sessionruntime.DecisionOutputRef, seq int64, payload json.RawMessage, limits sessionruntime.DecisionOutputLimits) (sessionruntime.DecisionOutputState, error) {
	state, err := b.MemoryBackend.AppendDecisionOutput(ctx, ref, seq, payload, limits)
	if err == nil && state.Applied {
		entry := struct {
			Type   string
			Output json.RawMessage
		}{Output: payload}
		if payload == nil {
			entry.Type = "decision_output_end"
		}
		b.output = append(b.output, entry)
	}
	return state, err
}

// ---------------------------------------------------------------------------
// driver table: one run, one ending, every entry
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------- table cases

func newDriveService(t *testing.T, pub *agenttest.EventRecorder, admitter runreq.TurnAdmitter, opts ...testOption) *Service {
	t.Helper()
	var options []testOption
	if admitter != nil {
		options = append(options, withTurnAdmitter(admitter))
	}
	if pub != nil {
		options = append(options, withDrive(func(d *drive.Deps) { d.Events = pub }))
	}
	return newTestService(t, Deps{}, append(options, opts...)...)
}

func continuationSessionRequest() runreq.ChatRequest {
	return runreq.ChatRequest{
		RunID: agenttest.LifecycleRunID, BotID: agenttest.LifecycleBotID, ChatID: agenttest.LifecycleBotID,
		ThreadID: agenttest.LifecycleSessionID, CurrentChannel: "web", UserMessagePersisted: true,
	}
}

// continuationService is a service whose decision continuations run against
// manager: the manager routes the answer, takes the run's publications, and
// records the run's ending.
func continuationService(t *testing.T, manager *sessionruntime.Manager, opts ...testOption) *Service {
	t.Helper()
	return newTestService(t, Deps{SessionRuntime: manager}, opts...)
}

// decisionCommand is the routed command that answers the decision the parked
// run of newWaitingDecisionRuntime waits on.
func decisionCommand(handle sessionruntime.RunHandle, id string) sessionruntime.Command {
	return sessionruntime.Command{
		StreamOutput: true, ID: id, TargetID: "decision-" + id, Type: sessionruntime.CommandUserInputResponse,
		BotID: handle.BotID, SessionID: handle.SessionID, RunID: handle.RunID, Generation: handle.Generation,
	}
}

// continueDecision drives one routed continuation the way the command handler
// does: on the context and handle the parked run's control hands out, with
// setup standing in for storing the answer and resolving the model run.
func continueDecision(t *testing.T, service *Service, manager *sessionruntime.Manager, command sessionruntime.Command, setup func(context.Context, runreq.ChatRequest) (drive.RunPlan, error)) {
	t.Helper()
	live := command
	live.Generation = strings.TrimSuffix(command.Generation, "-stale")
	runCtx, cancel, handle, err := manager.DecisionContinuationContext(live)
	if err != nil {
		t.Fatalf("decision continuation context: %v", err)
	}
	defer cancel()
	service.answer.DriveRouted(runCtx, command, handle, answer.Continuation{
		Req:   runreq.ChatRequest{BotID: handle.BotID, ChatID: handle.BotID, ThreadID: handle.SessionID, RunID: handle.RunID, RunHandle: handle, UserMessagePersisted: true},
		Setup: setup,
	})
}

// scriptedRun is a setup whose run produces events in order. A terminal event
// among them ends the run, and parks it when it carries a pending decision;
// cause, when set, is the failure the run reports besides its events.
func scriptedRun(events []native.StreamEvent, cause error) func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
	exec := &drivetest.ScriptedExecutor{Cause: cause}
	for _, ev := range events {
		if ev.IsTerminal() {
			terminal := ev
			exec.Terminal = &terminal
			exec.Deferred = ev.ApprovalID != "" || ev.UserInputID != ""
			continue
		}
		exec.Events = append(exec.Events, ev)
	}
	return func(context.Context, runreq.ChatRequest) (drive.RunPlan, error) {
		return drive.RunPlan{Exec: exec}, nil
	}
}

func newWaitingDecisionRuntime(t *testing.T, backends ...sessionruntime.Backend) (*sessionruntime.Manager, sessionruntime.RunHandle) {
	t.Helper()
	var backend sessionruntime.Backend = sessionruntime.NewMemoryBackend()
	if len(backends) > 0 {
		backend = backends[0]
	}
	manager := sessiontest.New(backend, sessionruntime.Options{
		OwnerID:       "runtime-lifecycle-owner",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Second,
		CommandAckTTL: time.Second,
	})
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("start runtime manager: %v", err)
	}
	handle, err := sessiontest.Start(context.Background(), manager,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		agenttest.LifecycleRunID,
		make(chan struct{}, 1),
		func() {},
		make(chan turn.InjectMessage, 1),
	)
	if err != nil {
		t.Fatalf("start runtime run: %v", err)
	}
	if _, err := manager.HandleAgentEvent(context.Background(), handle, native.StreamEvent{
		Type:        native.EventUserInputRequest,
		UserInputID: "44444444-4444-4444-8444-444444444444",
		Status:      "pending",
	}); err != nil {
		t.Fatalf("park runtime decision: %v", err)
	}
	if _, err := manager.FinishRun(context.Background(), handle, ""); err != nil {
		t.Fatalf("mark deferred producer ready: %v", err)
	}
	return manager, handle
}

type failNextRuntimeDecisionBackend struct {
	*sessionruntime.MemoryBackend
	failNext atomic.Bool
	err      error
}

func (b *failNextRuntimeDecisionBackend) Update(
	ctx context.Context,
	key sessionruntime.Key,
	update sessionruntime.SnapshotUpdate,
) (sessionruntime.Snapshot, bool, error) {
	if b.failNext.CompareAndSwap(true, false) {
		return sessionruntime.Snapshot{}, false, b.err
	}
	return b.MemoryBackend.Update(ctx, key, update)
}

// runFuncExecutor is an executor written inline by the test that needs one.
type runFuncExecutor func(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding

func (f runFuncExecutor) Execute(ctx context.Context, emit func(native.StreamEvent)) drive.ExecEnding {
	ending := f(ctx, emit)
	if ending.Commit == nil {
		ending.Commit = func(context.Context, *outcome.RunFailure, bool) (drive.CommitSummary, error) {
			return drive.CommitSummary{}, nil
		}
	}
	return ending
}

// lifecycleRow is one lifecycle row a run left, whichever write produced it:
// an unfenced run writes it at once, a fenced run when its terminal state is
// observed.
type lifecycleRow struct {
	Status   string
	Snapshot []byte
}

func lifecycleRows(store *agenttest.RecordingLifecycleStore) []lifecycleRow {
	var rows []lifecycleRow
	for _, row := range store.Creates {
		rows = append(rows, lifecycleRow{Status: row.Status, Snapshot: row.Snapshot})
	}
	for _, row := range store.TerminalUpserts {
		rows = append(rows, lifecycleRow{Status: row.Status, Snapshot: row.Snapshot})
	}
	return rows
}
