package application

import (
	"context"
	"errors"
	"log/slog"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/errs"
)

// runEndToStatus maps how a run ended onto the session runtime's live run
// status. A lost run has none: nothing is written for it. A parked run has
// none either: the session runtime keeps a run whose projection waits on a
// decision only when it is not told an ending, and a status, even
// waiting_decision, stops the run's commands so no answer could resume it.
func runEndToStatus(end runEnd) string {
	switch end {
	case endCompleted:
		return sessionruntime.RunStatusCompleted
	case endParked:
		return ""
	case endStopped:
		return sessionruntime.RunStatusAborted
	case endOwnershipLost:
		return ""
	default:
		return sessionruntime.RunStatusErrored
	}
}

// finishDrivenRun writes a run's terminal state once: the durable row, the
// live projection, and the run's context lifecycle. It returns the record when
// this write ended the run, and zero for a parked run, a lost run, a run
// nobody admitted, or a write that failed — which it records as an errlog
// event rather than raising, because by the time it runs there is no caller
// left who could act on it.
//
// The lifecycle write is part of this, not a callback the entry supplies: it
// has to be staged before the terminal write and completed after it, and two
// callers ordering that themselves is how the two orders drift apart.
func (s *Service) finishDrivenRun(ctx context.Context, run admittedRun, end runEnd, f *RunFailure) sessionruntime.TerminalRun {
	if s == nil || s.sessionRuntime == nil || run.Handle.FencingToken <= 0 || end == endOwnershipLost {
		return sessionruntime.TerminalRun{}
	}
	status := runEndToStatus(end)
	outcome := RunOutcome{Status: status}
	var code string
	var args map[string]string
	if f != nil {
		outcome.Cause = f.Public()
		code = string(f.Code)
		args = f.Args
	}
	cause := lifecycleCause(end, f)
	writeCtx := withRunOutcome(context.WithoutCancel(ctx), run.Handle.RunID, outcome, run.Mode)
	writeCtx, cancel := context.WithTimeout(writeCtx, terminalWriteTimeout)
	defer cancel()
	terminal, err := s.sessionRuntime.FinishRunWithFailure(writeCtx, run.Handle, status, code, args)
	switch {
	case err == nil:
		if status != "" || cause != nil {
			// A run whose own lifecycle terminal was never written (it failed
			// before the plan had one) still needs a row: the diagnostic table
			// is how a finished run that wrote no context is told apart from
			// one that wrote nothing at all.
			s.EnsureTerminalContextLifecycle(ctx, run.Handle.RunID, run.Handle.BotID, run.Handle.SessionID, cause)
		}
		return terminal
	case errors.Is(err, sessionruntime.ErrRunOwnershipLost):
		// Expected when this process was superseded mid-run: another owner
		// names the outcome and the reaper repairs the projection.
		s.logger.WarnContext(ctx, "terminal write refused after ownership loss",
			slog.String("run_id", run.Handle.RunID))
	default:
		s.logger.ErrorContext(ctx, "write run terminal state failed",
			slog.String("run_id", run.Handle.RunID),
			slog.String("bot_id", run.Handle.BotID),
			slog.String("session_id", run.Handle.SessionID),
			slog.Any("error", errs.Recorded(err)))
	}
	return sessionruntime.TerminalRun{}
}
