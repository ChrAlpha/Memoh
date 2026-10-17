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
	if h.events == nil {
		return nil
	}
	handle, ok := HandleFromContext(ctx)
	if !ok || strings.TrimSpace(handle.RunID) == "" || handle.FencingToken <= 0 {
		return nil
	}
	publishCtx := context.WithoutCancel(ctx)
	var lost atomic.Bool
	return func(event native.StreamEvent) native.SpawnRunObservation {
		if lost.Load() {
			return native.SpawnRunObservation{}
		}
		_, status, err := h.events.HandleAgentEventWithStatus(publishCtx, handle, outcome.PublicAgentStreamEvent(event))
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
