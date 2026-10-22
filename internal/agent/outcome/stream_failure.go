package outcome

import (
	"context"
	"errors"
	"strings"

	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/schedule"
)

// HistoryFailureCode is code when a failed turn's history records it, and empty
// otherwise.
func HistoryFailureCode(code apperror.Code) apperror.Code {
	switch code {
	case apperror.CodeAgentResponseTimeout,
		apperror.CodeAgentToolTimeout,
		apperror.CodeScheduleExecutionTimeout,
		apperror.CodeAgentResponseInterrupted,
		apperror.CodeAgentLoopDetected,
		apperror.CodeAgentProviderOverloaded,
		apperror.CodeAgentProviderRateLimited,
		apperror.CodeAgentProviderQuotaExhausted,
		apperror.CodeAgentProviderAuthFailed,
		apperror.CodeAgentProviderPermissionDenied,
		apperror.CodeAgentProviderRequestRejected,
		apperror.CodeAgentProviderUnreachable:
		return code
	default:
		return ""
	}
}

// ClassifyUserMessageHookError marks err when a user-message hook rejected the turn.
func ClassifyUserMessageHookError(err error) error {
	if err == nil || apperror.CodeOf(err) == apperror.CodeHookUserMessageFailed {
		return err
	}
	return apperror.Wrap(apperror.CodeHookUserMessageFailed, err, nil)
}

// AgentStreamFailure translates the failure an error event reports into the
// run's public error, keeping the event's cause, and returns nil for any other
// event. Every consumer of agent events uses it, so the run's terminal write,
// the history marker, the session's live view and the published event name a
// failure alike.
//
// An event that already carries a catalogued code keeps it and its args: the
// application and the External Agent runtimes name their failures themselves. A native
// failure is named from its cause by nativeFailureCode.
func AgentStreamFailure(event native.StreamEvent) error {
	if event.Type != native.EventError {
		return nil
	}
	if code := apperror.Code(strings.TrimSpace(event.Code)); code != "" {
		if _, ok := apperror.Lookup(code); ok {
			return apperror.Wrap(code, event.Cause, event.Args)
		}
	}
	return apperror.Wrap(nativeFailureCode(event.Cause), event.Cause, nil)
}

// nativeFailureCode names the failure a native run ended with. A provider
// that named why it refused the request is
// reported with that reason: an exhausted balance and a rejected key both need
// the user to go change something, and "the model response was interrupted,
// please try again" sends them back into a call that cannot succeed. A model
// call that got no response reports the provider unreachable. Anything else
// is an interrupted response; the runtime names its own failures itself.
func nativeFailureCode(cause error) apperror.Code {
	if code := providerFailureCode(cause, native.IsModelCallFailure(cause)); code != "" {
		return code
	}
	return apperror.CodeAgentResponseInterrupted
}

// AgentFailureStreamEvent is the error event that reports cause.
func AgentFailureStreamEvent(cause error) native.StreamEvent {
	code := classifyRunFailure(cause)
	definition, _ := apperror.Lookup(code)
	event := native.StreamEvent{
		Type:  native.EventError,
		Code:  string(code),
		Error: definition.Detail,
	}
	if apperror.CodeOf(cause) == code {
		if args := apperror.ArgsOf(cause); len(args) > 0 {
			event.Args = args
		}
	}
	return event
}

// PublicAgentStreamEvent is event as it leaves the application: an error
// event is replaced by its code and the catalog detail, so its cause never
// reaches a subscriber.
func PublicAgentStreamEvent(event native.StreamEvent) native.StreamEvent {
	if cause := AgentStreamFailure(event); cause != nil {
		return AgentFailureStreamEvent(cause)
	}
	return event
}

// AgentAbortCause is the cause a run that ended without naming a failure is
// reported with: the context's own cause, or the driver's stop marker when
// something outside the run ended it. Returning the marker rather than a bare
// error is what keeps "someone stopped this" distinguishable from "this run
// failed for a reason nobody named".
func AgentAbortCause(ctx context.Context) error {
	if ctx != nil {
		if cause := context.Cause(ctx); cause != nil {
			if errors.Is(cause, schedule.ErrExecutionTimeout) {
				return apperror.Wrap(apperror.CodeScheduleExecutionTimeout, cause, nil)
			}
			return cause
		}
	}
	return ErrRunStopped
}
