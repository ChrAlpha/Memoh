package application

import (
	"context"
	"errors"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/errs"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/schedule"
)

// runPhase names the stage a failure belongs to.
type runPhase uint8

const (
	phaseSetup     runPhase = iota + 1 // admitted, before the executor's first call
	phaseExecution                     // the executor is running
	phaseCommit                        // the ending's history write
	phasePublish                       // a publication was refused
)

// runEnd names how a run ended.
type runEnd uint8

const (
	endCompleted runEnd = iota + 1
	endParked           // waiting on a decision; the session runtime keeps the run
	endStopped          // someone stopped it
	endFailed
	endOwnershipLost // another owner names the outcome; nothing is written
)

// inputOwnership says who holds the input a run executes, which decides what a
// failure leaves behind. It is one fact with three values, not a per-entry
// policy: the entries that share a value behave alike.
type inputOwnership uint8

const (
	// inputHeld means the submitter still holds the input: a Web send whose
	// draft the composer can restore. A failure before execution starts writes
	// nothing, so the user can resend without duplicating a turn; a failure
	// during execution writes the turn.
	inputHeld inputOwnership = iota + 1
	// inputAccepted means the input left the submitter: an IM message already
	// sent to the platform, a Web retry or edit whose original is already in
	// history. Every failure at or after setup leaves the message and a row.
	inputAccepted
	// inputSynthetic means nobody submitted this turn: a schedule fire, an
	// interrupted-session resume. The fire's log carries the outcome; no user
	// message or failure row is written.
	inputSynthetic
)

// RunFailure is a run's one failure value. Every place that reports the
// failure reads it.
type RunFailure struct {
	Code  apperror.Code
	Args  map[string]string // only args the catalog entry allows
	Cause error             // private: result record and span only
	Phase runPhase
}

// Public returns the public error form of the failure: its text is the code,
// and the cause stays behind it for the result record and the span.
func (f RunFailure) Public() error {
	return apperror.Wrap(f.Code, f.Cause, f.Args)
}

// Recorded is Public for a boundary that reports a failure a nested unit has
// already recorded: an outer record is then a WARN rather than a second
// report of the same failure.
//
// The marker goes on the cause rather than around the public error. ERROR's
// own text is its code, but a marker's text is the whole chain's, so wrapping
// the public error would put the agent's words into the error a schedule log
// stores and replays. The marker is still found: the chain walk reaches a
// public error's cause.
func (f RunFailure) Recorded() error {
	return apperror.Wrap(f.Code, errs.Recorded(f.Cause), f.Args)
}

// Fault returns the fault category of the failure.
func (f RunFailure) Fault() apperror.Fault {
	return errs.FaultOf(f.Public())
}

// endingFacts is what the driver knows when it names how the run ended.
type endingFacts struct {
	Phase            runPhase
	RunCtx           context.Context     // ownership is read from its cause; stopping from errRunStopped
	Declared         error               // first failure the run declared
	Hook             bool                // Declared came from the user-message hook
	StoppedAtDeclare bool                // the run was already stopped when Declared was recorded
	Terminal         *native.StreamEvent // nil when the producer ended without one
	Deferred         bool                // awaiting a decision
	Commit           error               // the ending's history write failed
	Lost             bool                // a publication or write reported ownership loss
}

// errRunStopped is the driver's own stop marker. Every external stop signal is
// translated into it, so a cancellation nobody asked for (a channel restart, a
// gRPC send failure) is not mistaken for a user stop.
var errRunStopped = errors.New("agent run stopped")

// runStopped is the one explicit-cancellation test.
func runStopped(runCtx context.Context) bool {
	return errors.Is(context.Cause(runCtx), errRunStopped)
}

// ownershipLost reports whether err says this process no longer owns the run:
// the session runtime revoked ownership, or a fenced write found the fence
// stale.
func ownershipLost(err error) bool {
	return errors.Is(err, sessionruntime.ErrRunOwnershipLost) ||
		errors.Is(err, runtimefence.ErrStale) ||
		errors.Is(err, sessionruntime.ErrCommandTargetNotActive)
}

// nameRunEnd is the run failure policy, shared by every entry. It is called
// twice: once before the ending's history write to decide whether that write
// leaves a failure row, and once after it to fold the write's own error in.
func nameRunEnd(f endingFacts) (runEnd, *RunFailure) {
	// Ownership lost: another owner names the outcome, so nothing is written
	// and nothing is reported. A fenced write that found its fence stale is the
	// same fact as a revoked ownership.
	if f.Lost || ownershipLost(context.Cause(f.RunCtx)) || ownershipLost(f.Declared) || ownershipLost(f.Commit) {
		return endOwnershipLost, nil
	}

	// Stopped before the first failure was declared, or with none declared at
	// all. A failure declared first keeps its name: the run had already failed
	// when someone stopped it.
	if runStopped(f.RunCtx) && (f.Declared == nil || f.StoppedAtDeclare) {
		return endStopped, nil
	}

	// The first declared failure names the run.
	if f.Declared != nil {
		failure := &RunFailure{Cause: f.Declared, Phase: f.Phase}
		failure.Code, failure.Args = runFailureCode(f.Declared, f.Phase, f.Hook)
		return endFailed, failure
	}

	// A failed history write ends the run even when the terminal event parked
	// it: the run cannot be resumed from a state that was never written.
	if f.Commit != nil {
		return endFailed, &RunFailure{
			Code:  codeFromError(f.Commit, phaseCommit, false),
			Args:  apperror.ArgsOf(f.Commit),
			Cause: f.Commit,
			Phase: phaseCommit,
		}
	}

	if f.Deferred {
		return endParked, nil
	}

	if f.Terminal != nil {
		switch f.Terminal.Type {
		case event.AgentEnd:
			return endCompleted, nil
		case event.AgentAbort:
			return endFailed, abortRunFailure(f)
		}
	}

	// No terminal event and nothing declared: the cause the runtime was
	// cancelled with, or an unnamed failure for a stream that ended without
	// saying how.
	if cause := context.Cause(f.RunCtx); cause != nil {
		code, args := runFailureCode(cause, f.Phase, false)
		return endFailed, &RunFailure{Code: code, Args: args, Cause: cause, Phase: phaseExecution}
	}
	return endFailed, &RunFailure{
		Code:  apperror.CodeRuntimeRunFailed,
		Cause: errors.New("agent stream ended without a terminal event"),
		Phase: phaseExecution,
	}
}

// abortRunFailure names the failure an agent_abort terminal reports. The
// runtime only knows it was cancelled from outside, so the reasons it cannot
// see — a budget, a stop, the driver's own abort — are read from the run
// context here.
func abortRunFailure(f endingFacts) *RunFailure {
	failure := &RunFailure{Phase: phaseExecution}
	switch f.Terminal.EndReason {
	case event.EndLoopDetected:
		failure.Code = apperror.CodeAgentLoopDetected
		failure.Cause = errors.New("agent loop detected")
	case event.EndCanceled:
		if cause := context.Cause(f.RunCtx); cause != nil {
			failure.Code, failure.Args = runFailureCode(cause, phaseExecution, false)
			failure.Cause = cause
			return failure
		}
		failure.Code = apperror.CodeRuntimeRunFailed
		failure.Cause = errors.New("agent run was canceled")
	default:
		failure.Code = apperror.CodeRuntimeRunFailed
		failure.Cause = errors.New("agent run failed")
	}
	return failure
}

// runFailureCode names the public code a declared failure is reported with.
func runFailureCode(cause error, phase runPhase, hook bool) (apperror.Code, map[string]string) {
	if code := publicFailureCode(cause); code != "" {
		return code, apperror.ArgsOf(cause)
	}
	if code := sentinelFailureCode(cause); code != "" {
		return code, apperror.ArgsOf(cause)
	}
	var epf externalPromptFailure
	if errors.As(cause, &epf) {
		return apperror.CodeRuntimePromptFailed, nil
	}
	return codeFromError(cause, phase, hook), nil
}

// sentinelFailureCode names the failures that carry no catalog entry of their
// own but are named by the sentinel their cause wraps.
func sentinelFailureCode(cause error) apperror.Code {
	switch {
	case errors.Is(cause, ErrResponseTimeout):
		return apperror.CodeAgentResponseTimeout
	case errors.Is(cause, ErrToolTimeout):
		return apperror.CodeAgentToolTimeout
	case errors.Is(cause, schedule.ErrExecutionTimeout):
		return apperror.CodeScheduleExecutionTimeout
	}
	return ""
}

// codeFromError returns the default code for an error in a given phase.
func codeFromError(_ error, phase runPhase, hook bool) apperror.Code {
	if hook {
		return apperror.CodeHookUserMessageFailed
	}
	switch phase {
	case phaseCommit:
		return apperror.CodeSessionHistoryInconsistent
	case phasePublish:
		return apperror.CodeSessionPublishFailed
	default:
		return apperror.CodeRuntimeRunFailed
	}
}

// wantsFailureRow says whether the ending leaves a user message and a failure
// row in conversation history.
func wantsFailureRow(o inputOwnership, end runEnd, f *RunFailure, hook, replacing bool) bool {
	if end != endFailed {
		return false
	}
	switch o {
	case inputSynthetic:
		// Nobody submitted this turn; the fire's log carries the outcome.
		return false
	case inputHeld:
		// A Web send: the draft can still be handed back before execution
		// starts, and must not be duplicated after it.
		if replacing || f == nil {
			return false
		}
		switch f.Phase {
		case phaseSetup:
			// Only the user-message hook: the input reached the server, was
			// refused by policy, and the user has to see why. Resolve and
			// workspace failures leave no row, which is what lets the composer
			// restore the draft.
			return hook
		case phaseExecution, phasePublish:
			return true
		default: // phaseCommit writes the thing that failed
			return false
		}
	case inputAccepted:
		// The input left the submitter. Every failure at or after setup leaves
		// the message and a row; a replacement turn keeps its own row only for
		// the codes that name a failure the user has to see.
		if f == nil {
			return false
		}
		if replacing {
			return f.Phase == phaseExecution && f.Code != "" && historyFailureCode(f.Code) == f.Code
		}
		return f.Phase != phaseCommit
	}
	return false
}

// failureStreamEvent constructs the error event that reports f.
func failureStreamEvent(f RunFailure) native.StreamEvent {
	return native.StreamEvent{
		Type:  event.Error,
		Code:  string(f.Code),
		Args:  f.Args,
		Error: f.Public().Error(),
	}
}

// externalPromptFailure marks an error from external.Driver.Prompt.
type externalPromptFailure struct{ error }

func (e externalPromptFailure) Unwrap() error { return e.error }
