package drive

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/runtimefence"
)

// finishWrite is one terminal write the runtime was asked for.
type finishWrite struct {
	status, code string
	args         map[string]string
}

type finishRecorder struct{ writes []finishWrite }

func (r *finishRecorder) FinishRunWithFailure(_ context.Context, _ sessionruntime.RunHandle, status, code string, args map[string]string) (sessionruntime.TerminalRun, error) {
	r.writes = append(r.writes, finishWrite{status: status, code: code, args: args})
	return sessionruntime.TerminalRun{State: status}, nil
}

func newFinishDriver(runtime Runtime) *Driver {
	logger := slog.New(slog.DiscardHandler)
	return New(Deps{Logger: logger, Runtime: runtime, Lifecycle: lifecycle.New(lifecycle.Deps{Logger: logger})})
}

func admittedForFinish(token int64) AdmittedRun {
	return AdmittedRun{
		Handle: sessionruntime.RunHandle{BotID: "bot-1", SessionID: "session-1", RunID: "run-1", FencingToken: token},
		Mode:   "chat",
	}
}

// Every ending the driver names, and every status a caller resolves itself,
// goes through the one write: a lost run and a run nobody admitted write
// nothing, a parked run is written without a status.
func TestFinishWritesEachTerminationOnce(t *testing.T) {
	t.Parallel()

	failure := &outcome.RunFailure{Code: apperror.CodeAgentProviderAuthFailed, Args: map[string]string{"k": "v"}, Cause: errors.New("401")}
	for _, tc := range []struct {
		name  string
		run   AdmittedRun
		term  Termination
		wants []finishWrite
	}{
		{
			name: "completed", run: admittedForFinish(1), term: fromEnd(outcome.EndCompleted, nil),
			wants: []finishWrite{{status: sessionruntime.RunStatusCompleted}},
		},
		{
			name: "failed carries the code and args", run: admittedForFinish(1), term: fromEnd(outcome.EndFailed, failure),
			wants: []finishWrite{{status: sessionruntime.RunStatusErrored, code: string(apperror.CodeAgentProviderAuthFailed), args: map[string]string{"k": "v"}}},
		},
		{
			name: "stopped", run: admittedForFinish(1), term: fromEnd(outcome.EndStopped, nil),
			wants: []finishWrite{{status: sessionruntime.RunStatusAborted}},
		},
		{
			name: "parked is written without a status", run: admittedForFinish(1), term: fromEnd(outcome.EndParked, nil),
			wants: []finishWrite{{}},
		},
		{name: "ownership lost writes nothing", run: admittedForFinish(1), term: fromEnd(outcome.EndOwnershipLost, nil)},
		{name: "an unadmitted run writes nothing", run: admittedForFinish(0), term: fromEnd(outcome.EndCompleted, nil)},
		{
			name: "a caller's status", run: admittedForFinish(1), term: FromOutcome(outcome.RunOutcome{Status: sessionruntime.RunStatusCompleted}),
			wants: []finishWrite{{status: sessionruntime.RunStatusCompleted}},
		},
		{
			name: "a caller's cause reaches the record as its code only", run: admittedForFinish(1),
			term:  FromOutcome(outcome.RunOutcome{Status: sessionruntime.RunStatusErrored, Cause: apperror.Wrap(apperror.CodeAgentProviderAuthFailed, errors.New("SECRET"), nil)}),
			wants: []finishWrite{{status: sessionruntime.RunStatusErrored, code: string(apperror.CodeAgentProviderAuthFailed)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := &finishRecorder{}
			terminal := newFinishDriver(runtime).Finish(context.Background(), tc.run, tc.term)
			if len(runtime.writes) != len(tc.wants) {
				t.Fatalf("writes = %+v, want %+v", runtime.writes, tc.wants)
			}
			for i, want := range tc.wants {
				got := runtime.writes[i]
				if got.status != want.status || got.code != want.code || len(got.args) != len(want.args) {
					t.Fatalf("write %d = %+v, want %+v", i, got, want)
				}
				if terminal.State != want.status {
					t.Fatalf("terminal = %+v, want the runtime's record", terminal)
				}
			}
		})
	}
}

// observingRuntime ends runs the way the session runtime does: the terminal
// observer settles the run's lifecycle row inside the write.
type observingRuntime struct{ lifecycle *lifecycle.Recorder }

func (r observingRuntime) FinishRunWithFailure(ctx context.Context, h sessionruntime.RunHandle, status, code string, _ map[string]string) (sessionruntime.TerminalRun, error) {
	state := map[string]string{
		sessionruntime.RunStatusCompleted: "completed",
		sessionruntime.RunStatusAborted:   "aborted",
		sessionruntime.RunStatusErrored:   "failed",
	}[status]
	terminal := sessionruntime.TerminalRun{RunID: h.RunID, BotID: h.BotID, SessionID: h.SessionID, FencingToken: h.FencingToken, State: state, ErrorCode: code, Applied: true}
	r.lifecycle.ReconcileTerminal(ctx, terminal)
	return terminal, nil
}

const (
	fencedBotID     = "00000000-0000-0000-0000-0000000000b1"
	fencedSessionID = "00000000-0000-0000-0000-0000000000c1"
	fencedRunID     = "00000000-0000-0000-0000-0000000000d1"
)

func fencedFinish(recorder *lifecycle.Recorder) (*Driver, context.Context, AdmittedRun) {
	d := New(Deps{Logger: slog.New(slog.DiscardHandler), Runtime: observingRuntime{recorder}, Lifecycle: recorder})
	handle := sessionruntime.RunHandle{BotID: fencedBotID, SessionID: fencedSessionID, RunID: fencedRunID, FencingToken: 3}
	ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{BotID: fencedBotID, SessionID: fencedSessionID, Token: 3})
	return d, ctx, AdmittedRun{Handle: handle, Mode: "chat"}
}

// A fenced run that failed before its plan had a lifecycle has its row when
// Finish returns: the candidate is staged before the write, so the terminal
// observer inside the write settles it. No reconcile pass runs.
func TestFinishWritesTheLifecycleRowOfASetupFailureBeforeReturning(t *testing.T) {
	t.Parallel()

	store := &agenttest.RecordingLifecycleStore{}
	recorder := lifecycle.New(lifecycle.Deps{Store: store, Logger: slog.New(slog.DiscardHandler)})
	d, ctx, run := fencedFinish(recorder)

	cause := apperror.New(apperror.CodeContextBudgetUnsatisfied, nil)
	d.Finish(ctx, run, fromEnd(outcome.EndFailed, &outcome.RunFailure{Cause: cause}))

	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("lifecycle rows after Finish = %d, want 1 without a reconcile pass", len(store.TerminalUpserts))
	}
	if got := store.TerminalUpserts[0].Status; got != lifecycle.StatusFailedBudget {
		t.Fatalf("lifecycle status = %q, want the cause's %q", got, lifecycle.StatusFailedBudget)
	}
}

// What the plan staged is the complete candidate; the minimal one Finish
// stages does not replace it.
func TestFinishKeepsTheCandidateThePlanStaged(t *testing.T) {
	t.Parallel()

	store := &agenttest.RecordingLifecycleStore{}
	recorder := lifecycle.New(lifecycle.Deps{Store: store, Logger: slog.New(slog.DiscardHandler)})
	d, ctx, run := fencedFinish(recorder)

	full := lifecycle.MinimalSnapshot()
	full.Version = 7
	recorder.PersistSnapshot(ctx, fencedRunID, fencedBotID, fencedSessionID, &full, nil, true)
	d.Finish(ctx, run, fromEnd(outcome.EndCompleted, nil))

	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("lifecycle rows = %d, want 1", len(store.TerminalUpserts))
	}
	var got contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(store.TerminalUpserts[0].Snapshot, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 7 {
		t.Fatalf("lifecycle snapshot version = %d, want the plan's complete candidate (7)", got.Version)
	}
}

// A run without a fence has nothing to stage; its row is written right after
// the terminal write, and only for a run that ended.
func TestFinishWritesTheLifecycleRowAfterTheWriteWhenThereIsNoFence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		aborted    bool
		admitter   *agenttest.LifecycleAdmitter
		status     string
		cause      error
		wantStatus string
		wantRows   int
	}{
		{"pre-context failure", false, &agenttest.LifecycleAdmitter{}, sessionruntime.RunStatusErrored, apperror.New(apperror.CodeWorkspaceUnreachable, nil), lifecycle.StatusFailedProvider, 1},
		{"explicit abort", true, &agenttest.LifecycleAdmitter{}, sessionruntime.RunStatusAborted, nil, lifecycle.StatusAborted, 1},
		{"decision pause", false, &agenttest.LifecycleAdmitter{}, "", nil, "", 0},
		{"lost fence", false, &agenttest.LifecycleAdmitter{FinishErr: sessionruntime.ErrRunOwnershipLost}, sessionruntime.RunStatusErrored, errors.New("late failure"), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &agenttest.RecordingLifecycleStore{}
			d := New(Deps{Logger: slog.New(slog.DiscardHandler), Runtime: tc.admitter, Lifecycle: lifecycle.New(lifecycle.Deps{Store: store})})
			run := AdmittedRun{Handle: sessionruntime.RunHandle{BotID: fencedBotID, SessionID: fencedSessionID, RunID: fencedRunID, FencingToken: 1}, Mode: "chat"}

			ctx := context.Background()
			if tc.aborted {
				var cancel context.CancelCauseFunc
				ctx, cancel = context.WithCancelCause(ctx)
				cancel(context.Canceled)
			}
			d.Finish(ctx, run, FromOutcome(outcome.RunOutcome{Status: tc.status, Cause: tc.cause}))

			if len(tc.admitter.Finishes) != 1 {
				t.Fatalf("terminal writes = %d, want 1", len(tc.admitter.Finishes))
			}
			if len(store.Creates) != tc.wantRows {
				t.Fatalf("lifecycle creates = %d, want %d", len(store.Creates), tc.wantRows)
			}
			if tc.wantRows > 0 && store.Creates[0].Status != tc.wantStatus {
				t.Fatalf("lifecycle status = %q, want %q", store.Creates[0].Status, tc.wantStatus)
			}
		})
	}
}

// finishRefusing refuses the terminal write the way a superseded owner's is.
type finishRefusing struct{ err error }

func (r finishRefusing) FinishRunWithFailure(context.Context, sessionruntime.RunHandle, string, string, map[string]string) (sessionruntime.TerminalRun, error) {
	return sessionruntime.TerminalRun{}, r.err
}

// A write refused for lost ownership leaves no candidate behind: the new owner
// writes under its own fence, so nothing would ever claim this one. Another
// failure keeps it for a repair pass under the same fence. The candidate shows
// in the repair: with it the row says failed_budget, without it the generic
// failure.
func TestFinishDiscardsTheCandidateOnlyWhenOwnershipIsLost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"ownership lost", sessionruntime.ErrRunOwnershipLost, lifecycle.StatusFailedProvider},
		{"other write failure", errors.New("database unavailable"), lifecycle.StatusFailedBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &agenttest.RecordingLifecycleStore{}
			recorder := lifecycle.New(lifecycle.Deps{Store: store, Logger: slog.New(slog.DiscardHandler)})
			d := New(Deps{Logger: slog.New(slog.DiscardHandler), Runtime: finishRefusing{tc.err}, Lifecycle: recorder})
			handle := sessionruntime.RunHandle{BotID: fencedBotID, SessionID: fencedSessionID, RunID: fencedRunID, FencingToken: 3}
			ctx := runtimefence.WithContext(context.Background(), runtimefence.Fence{BotID: fencedBotID, SessionID: fencedSessionID, Token: 3})
			budget := apperror.New(apperror.CodeContextBudgetUnsatisfied, nil)

			d.Finish(ctx, AdmittedRun{Handle: handle, Mode: "chat"}, fromEnd(outcome.EndFailed, &outcome.RunFailure{Cause: budget}))

			recorder.ReconcileTerminal(context.Background(), sessionruntime.TerminalRun{
				RunID: fencedRunID, BotID: fencedBotID, SessionID: fencedSessionID,
				FencingToken: 3, State: "failed", Applied: true,
			})
			if len(store.TerminalUpserts) != 1 {
				t.Fatalf("repair wrote %d rows, want 1", len(store.TerminalUpserts))
			}
			if got := store.TerminalUpserts[0].Status; got != tc.want {
				t.Fatalf("status repaired under the finishing fence = %q, want %q", got, tc.want)
			}
		})
	}
}
