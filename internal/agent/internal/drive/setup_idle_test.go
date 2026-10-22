package drive_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/drive"
	"github.com/felinics/memoh/internal/agent/internal/drive/drivetest"
	"github.com/felinics/memoh/internal/agent/internal/outcome"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
)

const testIdleWindow = time.Minute

// The model's silence window starts when the plan's Setup has returned. Work
// Setup does before the first model call, such as compacting the context,
// is not silence from the model and cannot time the run out.
func TestDriveSetupWorkIsNotModelSilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		terminal := agenttest.TerminalEvent(t, "done")
		exec := &drivetest.ScriptedExecutor{
			Events:   []native.StreamEvent{{Type: native.EventTextDelta, Delta: "hi"}},
			Terminal: &terminal,
		}
		svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withIdleWindow(testIdleWindow))
		spec := drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{})
		spec.Setup = func(ctx context.Context, _ runreq.ChatRequest) (drive.RunPlan, error) {
			select {
			case <-time.After(5 * testIdleWindow):
			case <-ctx.Done():
				return drive.RunPlan{}, context.Cause(ctx)
			}
			return drive.RunPlan{Exec: exec, Req: agenttest.TriggerRequest()}, nil
		}

		result := svc.Drive(t.Context(), spec)

		if result.End != outcome.EndCompleted {
			t.Fatalf("end = %s, want completed after a slow Setup (failure %#v)", result.End, result.Failure)
		}
	})
}

// A stop reaches a run that is still in Setup: it ends as a stop without ever
// reaching its executor, and the idle watchdog has nothing to say about it.
func TestDriveStopDuringSetupEndsTheRunBeforeItsExecutor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var executed atomic.Int32
		exec := &countingExecutor{runs: &executed}
		svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withIdleWindow(testIdleWindow))
		spec := drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{})
		spec.Setup = func(ctx context.Context, _ runreq.ChatRequest) (drive.RunPlan, error) {
			select {
			case <-time.After(5 * testIdleWindow):
				return drive.RunPlan{Exec: exec, Req: agenttest.TriggerRequest()}, nil
			case <-ctx.Done():
				return drive.RunPlan{}, context.Cause(ctx)
			}
		}
		runCtx, stop := context.WithCancelCause(t.Context())
		defer stop(nil)
		go func() {
			time.Sleep(2 * testIdleWindow)
			stop(outcome.ErrRunStopped)
		}()

		result := svc.Drive(runCtx, spec)

		if result.End != outcome.EndStopped {
			t.Fatalf("end = %s, want stopped (failure %#v)", result.End, result.Failure)
		}
		if executed.Load() != 0 {
			t.Fatalf("executor ran %d times, want none after a stop during Setup", executed.Load())
		}
	})
}

// A caller that cancels a run while the model is silent decides how it ended.
// The run is not reported as an idle timeout: a stop is a stop, and any other
// cause the caller gave stays the run's cause.
func TestDriveCallerCancellationWinsOverTheIdleTimeout(t *testing.T) {
	custom := errors.New("caller gave up")
	cases := []struct {
		name      string
		cause     error
		wantEnd   outcome.RunEnd
		wantCause error
	}{
		{"stop marker", outcome.ErrRunStopped, outcome.EndStopped, nil},
		{"caller's own cause", custom, outcome.EndFailed, custom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exec := &drivetest.ScriptedExecutor{BlockUntil: make(chan struct{})}
				svc := newDriveService(&agenttest.EventRecorder{}, &agenttest.ScriptedAdmitter{Started: true}, withIdleWindow(testIdleWindow))
				runCtx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				go func() {
					time.Sleep(testIdleWindow / 2)
					cancel(tc.cause)
				}()

				result := svc.Drive(runCtx, drivetest.ScheduledSpec(exec, &drivetest.RecordingDelivery{}))

				if result.End != tc.wantEnd {
					t.Fatalf("end = %s, want %s (failure %#v)", result.End, tc.wantEnd, result.Failure)
				}
				if result.Failure != nil {
					if result.Failure.Code == apperror.CodeAgentResponseTimeout {
						t.Fatal("a cancelled run was reported as an idle timeout")
					}
					if tc.wantCause != nil && !errors.Is(result.Failure.Cause, tc.wantCause) {
						t.Fatalf("failure cause = %v, want the caller's cause", result.Failure.Cause)
					}
				}
			})
		})
	}
}

// countingExecutor counts how many runs reached it.
type countingExecutor struct{ runs *atomic.Int32 }

func (e *countingExecutor) Execute(context.Context, func(native.StreamEvent)) drive.ExecEnding {
	e.runs.Add(1)
	return drive.ExecEnding{}
}
