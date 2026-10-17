package outcome

import (
	"context"
	"errors"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
)

// classifyRunFailure names the public code a run failure is reported with: the
// catalogued code the cause carries, or runtime_run_failed when it carries none
// the catalog knows. The default is this process's own unnamed failure, not a
// code chosen by the entry that started the run.
func classifyRunFailure(cause error) apperror.Code {
	if code := PublicFailureCode(cause); code != "" {
		return code
	}
	return apperror.CodeRuntimeRunFailed
}

// hasCatalogCode reports whether err carries an apperror whose code is in the
// catalog.
func hasCatalogCode(err error) bool {
	_, ok := apperror.Lookup(apperror.CodeOf(err))
	return ok
}

// PublicFailureCode is the catalogued code a cause carries, or the code
// ExternalAgentError gives it. It is empty when the cause has neither.
func PublicFailureCode(cause error) apperror.Code {
	public := ExternalAgentError(cause)
	if !hasCatalogCode(public) {
		return ""
	}
	return apperror.CodeOf(public)
}

// RunOutcome is how the owner of a run reports its end to the terminal write.
// Status is a session runtime run status, or empty to leave the outcome to the
// session runtime, which derives it from what the run recorded. Cause is the
// failure the owner holds, if any.
type RunOutcome struct {
	Status string
	Cause  error
}

// ErrorCode is the code the terminal write records for the run: the catalogued
// code the cause carries, or empty to let the session runtime name it. The code
// travels in its own column; it is never written as the run's error message.
func (o RunOutcome) ErrorCode() string {
	return string(PublicFailureCode(o.Cause))
}

// Resolve names the status of an ending whose owner reported only a cause: a
// failure the owner holds is an errored run, unless it is the plain
// cancellation of a run that was stopped; a run whose context has ended was
// aborted; anything else completed. An outcome that already has a status is
// returned as it is.
func (o RunOutcome) Resolve(runCtx context.Context) RunOutcome {
	if strings.TrimSpace(o.Status) != "" {
		return o
	}
	failureCause := o.Cause
	if privateCause := apperror.CauseOf(o.Cause); privateCause != nil {
		failureCause = privateCause
	}
	explicitlyCanceled := o.Cause != nil &&
		errors.Is(failureCause, context.Canceled) &&
		errors.Is(context.Cause(runCtx), context.Canceled)
	switch {
	case o.Cause != nil && !explicitlyCanceled:
		return RunOutcome{Status: sessionruntime.RunStatusErrored, Cause: o.Cause}
	case runCtx.Err() != nil:
		return RunOutcome{Status: sessionruntime.RunStatusAborted}
	default:
		return RunOutcome{Status: sessionruntime.RunStatusCompleted}
	}
}
