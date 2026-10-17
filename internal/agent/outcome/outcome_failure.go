package outcome

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

// RunPhase names the stage a failure belongs to.
type RunPhase uint8

const (
	PhaseSetup     RunPhase = iota + 1 // admitted, before the executor's first call
	PhaseExecution                     // the executor is running
	PhaseCommit                        // the ending's history write
	PhasePublish                       // a publication was refused
)

// RunEnd names how a run ended.
type RunEnd uint8

const (
	EndCompleted RunEnd = iota + 1
	EndParked           // waiting on a decision; the session runtime keeps the run
	EndStopped          // someone stopped it
	EndFailed
	EndOwnershipLost // another owner names the outcome; nothing is written
)

// InputOwnership says who holds the input a run executes, which decides what a
// failure leaves behind. It is one fact with three values, not a per-entry
// policy: the entries that share a value behave alike.
type InputOwnership uint8

const (
	// InputHeld means the submitter still holds the input: a Web send whose
	// draft the composer can restore. A failure before execution starts writes
	// nothing, so the user can resend without duplicating a turn; a failure
	// during execution writes the turn.
	InputHeld InputOwnership = iota + 1
	// InputAccepted means the input left the submitter: an IM message already
	// sent to the platform, a Web retry or edit whose original is already in
	// history. Every failure at or after setup leaves the message and a row.
	InputAccepted
	// InputSynthetic means nobody submitted this turn: a schedule fire, an
	// interrupted-session resume. The fire's log carries the outcome; no user
	// message or failure row is written.
	InputSynthetic
)

// RunFailure is a run's one failure value. Every place that reports the
// failure reads it.
type RunFailure struct {
	Code  apperror.Code
	Args  map[string]string // only args the catalog entry allows
	Cause error             // private: result record and span only
	Phase RunPhase
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

// EndingFacts is what the driver knows when it names how the run ended.
type EndingFacts struct {
	Phase            RunPhase
	RunCtx           context.Context     // ownership is read from its cause; stopping from errRunStopped
	Declared         error               // first failure the run declared
	Hook             bool                // Declared came from the user-message hook
	StoppedAtDeclare bool                // the run was already stopped when Declared was recorded
	Terminal         *native.StreamEvent // nil when the producer ended without one
	Deferred         bool                // awaiting a decision
	Commit           error               // the ending's history write failed
	Lost             bool                // a publication or write reported ownership loss
}

// ErrRunStopped is the driver's own stop marker. Every external stop signal is
// translated into it, so a cancellation nobody asked for (a channel restart, a
// gRPC send failure) is not mistaken for a user stop.
var ErrRunStopped = errors.New("agent run stopped")

// RunStopped is the one explicit-cancellation test.
func RunStopped(runCtx context.Context) bool {
	return errors.Is(context.Cause(runCtx), ErrRunStopped)
}

// RunContextEnding names the ending a run context inherits from its caller. A
// plain cancellation is a stop: the caller that started the run went away, or
// asked for it to stop. Every other cause names its own reason — a revoked
// ownership, an execution budget, a deadline — and is kept as it is.
func RunContextEnding(cause error) error {
	if cause == nil || errors.Is(cause, context.Canceled) {
		return ErrRunStopped
	}
	return cause
}

// OwnershipLost reports whether err says this process no longer owns the run:
// the session runtime revoked ownership, or a fenced write found the fence
// stale.
func OwnershipLost(err error) bool {
	return errors.Is(err, sessionruntime.ErrRunOwnershipLost) ||
		errors.Is(err, runtimefence.ErrStale) ||
		errors.Is(err, sessionruntime.ErrCommandTargetNotActive)
}

// NameRunEnd is the run failure policy, shared by every entry. It is called
// twice: once before the ending's history write to decide whether that write
// leaves a failure row, and once after it to fold the write's own error in.
func NameRunEnd(f EndingFacts) (RunEnd, *RunFailure) {
	// Ownership lost: another owner names the outcome, so nothing is written
	// and nothing is reported. A fenced write that found its fence stale is the
	// same fact as a revoked ownership.
	if f.Lost || OwnershipLost(context.Cause(f.RunCtx)) || OwnershipLost(f.Declared) || OwnershipLost(f.Commit) {
		return EndOwnershipLost, nil
	}

	// Stopped before the first failure was declared, or with none declared at
	// all. A failure declared first keeps its name: the run had already failed
	// when someone stopped it.
	if RunStopped(f.RunCtx) && (f.Declared == nil || f.StoppedAtDeclare) {
		return EndStopped, nil
	}

	// The first declared failure names the run.
	if f.Declared != nil {
		failure := &RunFailure{Cause: f.Declared, Phase: f.Phase}
		failure.Code, failure.Args = RunFailureCode(f.Declared, f.Phase, f.Hook)
		return EndFailed, failure
	}

	// A failed history write ends the run even when the terminal event parked
	// it: the run cannot be resumed from a state that was never written.
	if f.Commit != nil {
		return EndFailed, &RunFailure{
			Code:  CodeFromError(f.Commit, PhaseCommit, false),
			Args:  apperror.ArgsOf(f.Commit),
			Cause: f.Commit,
			Phase: PhaseCommit,
		}
	}

	if f.Deferred {
		return EndParked, nil
	}

	if f.Terminal != nil {
		switch f.Terminal.Type {
		case event.AgentEnd:
			return EndCompleted, nil
		case event.AgentAbort:
			return EndFailed, abortRunFailure(f)
		}
	}

	// No terminal event and nothing declared: the cause the runtime was
	// cancelled with, or an unnamed failure for a stream that ended without
	// saying how.
	if cause := context.Cause(f.RunCtx); cause != nil {
		code, args := RunFailureCode(cause, f.Phase, false)
		return EndFailed, &RunFailure{Code: code, Args: args, Cause: cause, Phase: PhaseExecution}
	}
	return EndFailed, &RunFailure{
		Code:  apperror.CodeRuntimeRunFailed,
		Cause: errors.New("agent stream ended without a terminal event"),
		Phase: PhaseExecution,
	}
}

// abortRunFailure names the failure an agent_abort terminal reports. The
// runtime only knows it was cancelled from outside, so the reasons it cannot
// see — a budget, a stop, the driver's own abort — are read from the run
// context here.
func abortRunFailure(f EndingFacts) *RunFailure {
	failure := &RunFailure{Phase: PhaseExecution}
	switch f.Terminal.EndReason {
	case event.EndLoopDetected:
		failure.Code = apperror.CodeAgentLoopDetected
		failure.Cause = errors.New("agent loop detected")
	case event.EndCanceled:
		if cause := context.Cause(f.RunCtx); cause != nil {
			failure.Code, failure.Args = RunFailureCode(cause, PhaseExecution, false)
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

// RunFailureCode names the public code a declared failure is reported with.
func RunFailureCode(cause error, phase RunPhase, hook bool) (apperror.Code, map[string]string) {
	if code := PublicFailureCode(cause); code != "" {
		return code, apperror.ArgsOf(cause)
	}
	if code := sentinelFailureCode(cause); code != "" {
		return code, apperror.ArgsOf(cause)
	}
	var epf ExternalPromptFailure
	if errors.As(cause, &epf) {
		return apperror.CodeRuntimePromptFailed, nil
	}
	return CodeFromError(cause, phase, hook), nil
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

// CodeFromError returns the default code for an error in a given phase.
func CodeFromError(_ error, phase RunPhase, hook bool) apperror.Code {
	if hook {
		return apperror.CodeHookUserMessageFailed
	}
	switch phase {
	case PhaseCommit:
		return apperror.CodeSessionHistoryInconsistent
	case PhasePublish:
		return apperror.CodeSessionPublishFailed
	default:
		return apperror.CodeRuntimeRunFailed
	}
}

// WantsFailureRow says whether the ending leaves a user message and a failure
// row in conversation history.
func WantsFailureRow(o InputOwnership, end RunEnd, f *RunFailure, hook, replacing bool) bool {
	if end != EndFailed {
		return false
	}
	switch o {
	case InputSynthetic:
		// Nobody submitted this turn; the fire's log carries the outcome.
		return false
	case InputHeld:
		// A Web send: the draft can still be handed back before execution
		// starts, and must not be duplicated after it.
		if replacing || f == nil {
			return false
		}
		switch f.Phase {
		case PhaseSetup:
			// Only the user-message hook: the input reached the server, was
			// refused by policy, and the user has to see why. Resolve and
			// workspace failures leave no row, which is what lets the composer
			// restore the draft.
			return hook
		case PhaseExecution, PhasePublish:
			return true
		default: // phaseCommit writes the thing that failed
			return false
		}
	case InputAccepted:
		// The input left the submitter. Every failure at or after setup leaves
		// the message and a row; a replacement turn keeps its own row only for
		// the codes that name a failure the user has to see.
		if f == nil {
			return false
		}
		if replacing {
			return f.Phase == PhaseExecution && f.Code != "" && HistoryFailureCode(f.Code) == f.Code
		}
		return f.Phase != PhaseCommit
	}
	return false
}

// FailureStreamEvent constructs the error event that reports f.
func FailureStreamEvent(f RunFailure) native.StreamEvent {
	return native.StreamEvent{
		Type:  event.Error,
		Code:  string(f.Code),
		Args:  f.Args,
		Error: f.Public().Error(),
	}
}

// ExternalPromptFailure marks an error from external.Driver.Prompt.
type ExternalPromptFailure struct{ error }

// NewExternalPromptFailure marks err as an error from external.Driver.Prompt.
func NewExternalPromptFailure(err error) ExternalPromptFailure { return ExternalPromptFailure{err} }

// Unwrap returns the runtime's error.
func (e ExternalPromptFailure) Unwrap() error { return e.error }

// RunOwnershipLost reports that this process was told, mid-run, that it no
// longer owns the run.
//
// It is the one interruption whose output must not be written. Every other stop
// — client disconnect, idle timeout, user abort — leaves this process still
// entitled to say how the turn ended, so persisting what it has is the honest
// record. Ownership loss does not: another incarnation of the runtime now
// decides that run's outcome, and a superseded owner writing history would
// attach output to a turn whose ending it can no longer name (SR-DUR-002).
//
// The distinction only survives because the revocation carries a cause. On the
// wire both arrive as the same cancelled context.
func RunOwnershipLost(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), sessionruntime.ErrRunOwnershipLost)
}

// String names a RunEnd for a log line.
func (e RunEnd) String() string {
	switch e {
	case EndCompleted:
		return "completed"
	case EndParked:
		return "parked"
	case EndStopped:
		return "stopped"
	case EndFailed:
		return "failed"
	case EndOwnershipLost:
		return "ownership_lost"
	default:
		return "unknown"
	}
}
