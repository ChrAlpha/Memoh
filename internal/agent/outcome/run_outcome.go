package outcome

import (
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
