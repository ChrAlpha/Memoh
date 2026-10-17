package drive

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/lifecycle"
	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/errs"
)

// runEndToStatus maps how a run ended onto the session runtime's live run
// status. A lost run has none: nothing is written for it. A parked run has
// none either: the session runtime keeps a run whose projection waits on a
// decision only when it is not told an ending, and a status, even
// waiting_decision, stops the run's commands so no answer could resume it.
func runEndToStatus(end outcome.RunEnd) string {
	switch end {
	case outcome.EndCompleted:
		return sessionruntime.RunStatusCompleted
	case outcome.EndParked:
		return ""
	case outcome.EndStopped:
		return sessionruntime.RunStatusAborted
	case outcome.EndOwnershipLost:
		return ""
	default:
		return sessionruntime.RunStatusErrored
	}
}

// TerminalWriteTimeout bounds the terminal write. It runs after the run's own
// context is already canceled, so it cannot inherit that deadline, and it must
// not be able to hold a pump goroutine open indefinitely either.
const TerminalWriteTimeout = 10 * time.Second

// Termination describes how an admitted run ended, in the terms the terminal
// write records. The driver names it for the runs it executes; FromOutcome
// names it for a run whose caller resolved the status itself.
type Termination struct {
	// result is what the run's result record reads: the status and the cause.
	result outcome.RunOutcome
	// code and args name the failure on the durable row and in the live view.
	code string
	args map[string]string
	// lifecycleCause is what the run's context lifecycle records.
	lifecycleCause error
	// lost says another owner names the run, so nothing is written for it.
	lost bool
}

// fromEnd is the termination of a run the driver executed: how it ended and
// the failure that named it.
func fromEnd(end outcome.RunEnd, f *outcome.RunFailure) Termination {
	t := Termination{
		result:         outcome.RunOutcome{Status: runEndToStatus(end)},
		lifecycleCause: lifecycleCause(end, f),
		lost:           end == outcome.EndOwnershipLost,
	}
	if f != nil {
		t.result.Cause = f.Public()
		t.code = string(f.Code)
		t.args = f.Args
	}
	return t
}

// FromOutcome is the termination of a run whose caller resolved the status
// itself, as an admitted run does that the driver never executed (a discuss
// batch that was already covered, a spawned agent). Only the catalogued code of
// the cause reaches the record; the cause itself is a private diagnostic.
func FromOutcome(ended outcome.RunOutcome) Termination {
	cause := ended.Cause
	switch {
	case cause == nil && ended.Status == sessionruntime.RunStatusAborted:
		cause = context.Canceled
	case cause == nil && ended.Status == sessionruntime.RunStatusErrored:
		cause = errors.New("run finished with an unspecified error")
	}
	return Termination{
		result:         ended,
		code:           ended.ErrorCode(),
		lifecycleCause: cause,
	}
}

// FinishRun writes a run's terminal state once: the durable row, the live
// projection, and the run's context lifecycle. It is the only place a run's
// terminal state is written. It returns the record when this write ended the
// run, and zero for a parked run, a lost run, a run nobody admitted, or a write
// that failed, which it records as an errlog event rather than raising,
// because by the time it runs there is no caller left who could act on it.
//
// The lifecycle write is part of this, not a callback the entry supplies. A
// content-light candidate is staged before the terminal write, so the terminal
// observer that runs inside the write settles it; a candidate the run's plan
// staged already is better and stays. A run that cannot stage one, because it
// has no fence, gets its row written after the terminal write.
func (d *Driver) FinishRun(ctx context.Context, run AdmittedRun, t Termination) sessionruntime.TerminalRun {
	if d.runtime == nil || run.Handle.FencingToken <= 0 || t.lost {
		return sessionruntime.TerminalRun{}
	}
	handle := run.Handle
	status := t.result.Status
	// A run whose own lifecycle terminal was never written (it failed before
	// the plan had one) still needs a row: the diagnostic table is how a
	// finished run that wrote no context is told apart from one that wrote
	// nothing at all. A parked run has ended nothing and needs none.
	needsRow := status != "" || t.lifecycleCause != nil
	staged := false
	if needsRow {
		minimal := lifecycle.MinimalSnapshot()
		staged = d.lifecycle.StageCandidate(ctx, handle.RunID, handle.BotID, handle.SessionID, &minimal, t.lifecycleCause, lifecycle.CandidateMinimal)
	}
	writeCtx := outcome.WithRunOutcomeInMode(context.WithoutCancel(ctx), handle.RunID, t.result, run.Mode)
	writeCtx, cancel := context.WithTimeout(writeCtx, TerminalWriteTimeout)
	defer cancel()
	terminal, err := d.runtime.FinishRunWithFailure(writeCtx, handle, status, t.code, t.args)
	switch {
	case err == nil:
		if needsRow && !staged {
			d.lifecycle.EnsureTerminal(ctx, handle.RunID, handle.BotID, handle.SessionID, t.lifecycleCause)
		}
		return terminal
	case errors.Is(err, sessionruntime.ErrRunOwnershipLost):
		// Expected when this process was superseded mid-run: another owner
		// names the outcome and the reaper repairs the projection.
		d.logger.WarnContext(ctx, "terminal write refused after ownership loss",
			slog.String("run_id", handle.RunID))
	default:
		d.logger.ErrorContext(ctx, "write run terminal state failed",
			slog.String("run_id", handle.RunID),
			slog.String("bot_id", handle.BotID),
			slog.String("session_id", handle.SessionID),
			slog.String("status", status),
			slog.Any("error", errs.Recorded(err)))
	}
	return sessionruntime.TerminalRun{}
}
