package native

import (
	"errors"
)

// codeRunFailed is the catalog code of a failure of the runtime's own work.
// internal/apperror registers the same value; the runtime keeps its own
// constant, as the other writers of run codes do.
const codeRunFailed = "runtime_run_failed"

// localFailureEvent is the error event for a failure of the runtime's own
// work: a hook, the dispatch, a commit or checkpoint, a tool batch or its
// approval. It names its code, so the failure is not reported as an
// interrupted model response.
func localFailureEvent(err error) StreamEvent {
	return StreamEvent{Type: EventError, Code: codeRunFailed, Cause: err}
}

// turnHookError is the turn hook's error for the failure a run ended with:
// the code the failure event names. A hook command runs in the bot's
// workspace, so the internal text of a local failure stays out of it. A model call failure names no code in
// the runtime, which leaves classifying the provider's answer to the
// application; the hook still receives its text.
func turnHookError(failure StreamEvent) string {
	if failure.Code != "" {
		return failure.Code
	}
	if IsModelCallFailure(failure.Cause) {
		return failure.Cause.Error()
	}
	return codeRunFailed
}

// modelCallFailure marks the failure of a model provider call the runtime
// made. It changes neither the text nor the chain of the failure.
type modelCallFailure struct {
	err error
}

func (f *modelCallFailure) Error() string { return f.err.Error() }

func (f *modelCallFailure) Unwrap() error { return f.err }

// IsModelCallFailure reports whether err is, or wraps, the failure of a model
// provider call the runtime made. A failure of the runtime's own work, such as
// a tool batch or an approval handler, is not one, even when a network error
// caused it, so a caller can tell a provider that cannot be reached from a
// local request that failed.
func IsModelCallFailure(err error) bool {
	var failure *modelCallFailure
	return errors.As(err, &failure)
}
