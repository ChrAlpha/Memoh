package admit

import (
	"context"
	"fmt"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
)

// TriggeredView lets a triggered caller inject the subscriber-facing
// projection for its run. The hook fires at activation, when the handle already
// carries the allocated turn identity, which is why the view is a factory
// rather than a value: the turn id only exists by then.
// Nil means the run projects no request user turn (the historical default).
type TriggeredView func(handle sessionruntime.RunHandle) *sessionruntime.RunAdmissionView

// Trigger names a run the server starts itself: a schedule fire, a discuss
// batch, a spawned agent, a resumed run.
type Trigger struct {
	BotID    string
	ThreadID string
	// InvocationID is the retry identity of the run; admitting it twice is a
	// conflict.
	InvocationID string
	// Submission is what the run was asked to do. Its fingerprint decides
	// whether a repeated invocation id is the same submission.
	Submission []byte
	// View optionally supplies the subscriber-facing admission view (the
	// request user turn projection); nil keeps the run contentless in the
	// projection, the historical default for triggers and subagents alike.
	View TriggeredView
	// StopCause is what a routed abort ends the run context with. A run the
	// driver executes names its ending from the driver's own stop marker; a
	// run another layer executes keeps the plain cancellation it already reads.
	StopCause error
	// Mode is the session mode the run is admitted in. A resumed run takes the
	// interrupted run's.
	Mode string
	// DiscussCoverPosition is the history position a discuss run's batch ends
	// at: set for a discuss run and for a resumed one that continues it.
	DiscussCoverPosition *int64
	// ResumeRunID names the interrupted run a resumed run continues.
	ResumeRunID string
	// JoinTurn is the existing turn the run answers; nil opens a new one.
	JoinTurn *ledger.Turn
}

// Triggered admits a run the server starts itself and returns the context to
// execute it in, the admission, and release, which ends the admission.
//
// These callers reach the agent directly rather than through StartTurn, but they
// occupy a thread exactly like any other turn, so they take the same durable slot
// on the same terms. The returned context is cancelable so a routed abort can
// stop a long run, and so a lost owner lease revokes execution instead of
// letting a superseded owner keep writing.
//
// Admission does not write the run's terminal state. The caller ends the run
// through the driver and then calls release; a run that ends without the
// driver's write stays active until the reaper times it out.
func (a *Admitter) Triggered(ctx context.Context, t Trigger) (context.Context, sessionruntime.Admission, func(), error) {
	runCtx, cancelCause := context.WithCancelCause(ctx)
	admission, err := a.runtime.Admit(runCtx, sessionruntime.AdmitInput{
		BotID:        t.BotID,
		SessionID:    t.ThreadID,
		InvocationID: t.InvocationID,
		Payload:      t.Submission,
		Mode:         t.Mode,
		ResumeRunID:  t.ResumeRunID,
		JoinTurn:     t.JoinTurn,

		DiscussCoverPosition: t.DiscussCoverPosition,
		Execution: sessionruntime.Execution{
			Admission: func(_ context.Context, handle sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
				if t.View == nil {
					return sessionruntime.RunAdmissionView{}, nil
				}
				if view := t.View(handle); view != nil {
					return *view, nil
				}
				return sessionruntime.RunAdmissionView{}, nil
			},
			Cancel:          func() { cancelCause(t.StopCause) },
			OwnershipCancel: cancelCause,
		},
	})
	if err != nil {
		cancelCause(context.Canceled)
		return nil, sessionruntime.Admission{}, nil, err
	}
	if !admission.Started {
		// A trigger that finds its own invocation already admitted has nothing to
		// do: the run exists and either someone owns it or it has finished.
		cancelCause(context.Canceled)
		return nil, sessionruntime.Admission{}, nil, fmt.Errorf("%w: %s", sessionruntime.ErrInvocationConflict, t.InvocationID)
	}
	runCtx = a.Fence(runCtx, admission)
	return runCtx, admission, func() { cancelCause(context.Canceled) }, nil
}
