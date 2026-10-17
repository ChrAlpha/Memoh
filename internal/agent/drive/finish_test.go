package drive

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
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
func TestFinishRunWritesEachTerminationOnce(t *testing.T) {
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
			terminal := newFinishDriver(runtime).FinishRun(context.Background(), tc.run, tc.term)
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

// A driver without a runtime writes nothing: an admitted run's terminal state
// is the runtime's to record.
func TestFinishRunWithoutARuntimeWritesNothing(t *testing.T) {
	t.Parallel()

	driver := newFinishDriver(nil)
	if terminal := driver.FinishRun(context.Background(), admittedForFinish(1), fromEnd(outcome.EndCompleted, nil)); terminal.State != "" {
		t.Fatalf("terminal = %+v, want none", terminal)
	}
}
