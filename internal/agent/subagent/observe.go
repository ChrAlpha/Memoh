package subagent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/apperror"
)

// Failure is the error a spawned attempt that ended with event reports
// to the spawn provider: the run's public failure, named as for any native run,
// with the event's cause in its chain.
func Failure(event native.StreamEvent) error {
	return outcome.AgentStreamFailure(event)
}

// Observer returns a per-event publisher that feeds one spawned
// agent run's stream into the session runtime, or nil when there is nothing to
// publish to — no runtime configured, or a context without an admitted handle.
//
// The returned function mirrors forwardWSStreamEvents' publishing discipline:
// events leave as outcome.PublicAgentStreamEvent makes them, so a failed spawned run
// is named like any other native run; they are published on a context that
// survives the run's cancellation (an aborted run's final events are exactly
// the ones a subscriber must see); and a lost ownership stops publishing
// outright because every later event would fail identically.
func (h *Host) Observer(ctx context.Context) native.SpawnRunObserver {
	run, ok := runFromContext(ctx)
	if !ok || strings.TrimSpace(run.handle.RunID) == "" || run.handle.FencingToken <= 0 {
		return nil
	}
	handle := run.handle
	publishCtx := context.WithoutCancel(ctx)
	var lost atomic.Bool
	// declared is the failure the last error event published names, which is
	// the failure the session runtime records for the run when it ends.
	var declared *outcome.RunFailure
	return func(event native.StreamEvent) native.SpawnRunObservation {
		if lost.Load() {
			return native.SpawnRunObservation{}
		}
		public := outcome.PublicAgentStreamEvent(event)
		switch event.Type {
		case native.EventError:
			declared = declaredFailure(public, event.Cause)
		case native.EventAgentEnd, native.EventAgentAbort:
			// History comes before the terminal event, as it does for a driven
			// run: the failure is named on the checkpoint the run wrote, and the
			// committer closes, before the terminal is published. After it the
			// run is finishing and its rows are no longer writable.
			run.endHistory(publishCtx, declared, h.logger)
		}
		_, status, err := h.events.HandleAgentEventWithStatus(publishCtx, handle, public)
		if err != nil {
			if errors.Is(err, sessionruntime.ErrRunOwnershipLost) {
				lost.Store(true)
			}
			h.logger.WarnContext(ctx, "publish subagent runtime event failed",
				slog.Any("error", err),
				slog.String("bot_id", handle.BotID),
				slog.String("session_id", handle.SessionID),
				slog.String("run_id", handle.RunID),
			)
			return native.SpawnRunObservation{}
		}
		if event.Type != native.EventAgentEnd && event.Type != native.EventAgentAbort {
			return native.SpawnRunObservation{}
		}
		observation := native.SpawnRunObservation{TerminalResolved: true}
		switch status {
		case sessionruntime.RunStatusCompleted:
			observation.TerminalOutcome = tools.SpawnAttemptCompleted
		case sessionruntime.RunStatusAborted:
			observation.TerminalOutcome = tools.SpawnAttemptAbort
		case sessionruntime.RunStatusErrored:
			observation.TerminalOutcome = tools.SpawnAttemptFailure
		default:
			return native.SpawnRunObservation{}
		}
		return observation
	}
}

// declaredFailure is the failure an error event names, in the terms the
// session runtime records it: the event's code, or the unnamed failure.
func declaredFailure(public native.StreamEvent, cause error) *outcome.RunFailure {
	code := apperror.Code(strings.TrimSpace(public.Code))
	args := public.Args
	if code == "" {
		code, args = apperror.CodeRuntimeRunFailed, nil
	}
	return &outcome.RunFailure{Code: code, Args: args, Cause: cause, Phase: outcome.PhaseExecution}
}
