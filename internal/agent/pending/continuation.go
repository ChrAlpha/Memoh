package pending

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Continuation is the unfinished work an interrupted run left on its session.
type Continuation struct {
	// Run is the interrupted run whose resume intent is still recorded.
	Run dbsqlc.SessionRun
	// WorkspaceTargetID is the workspace target the continuation runs on; empty
	// is the session's default.
	WorkspaceTargetID string
}

// ErrContinuationsBusy is what Continuations.Start returns when the
// implementation has no capacity for another continuation. It is a reason to
// wait, not to give the continuation up, and not a failure worth a warning.
var ErrContinuationsBusy = errors.New("too many continuations are running")

// Continuations is the port to the interrupted runs sessions are owed a
// continuation of. Whether and when to start one is not its business: pending
// asks once per admission and decides the order.
type Continuations interface {
	// Owed returns the continuation the session is owed. found is false when it
	// owes none. A continuation that can never run is returned with an error
	// wrapping runreq.ErrResumeUnrecoverable, so the caller can retire it.
	Owed(ctx context.Context, key sessionruntime.Key) (c Continuation, found bool, err error)
	// Start admits the continuation as a run of its own and drives it. done
	// closes when that run has ended; the run's end asks pending again, so a
	// caller that does not follow the run ignores it. An error wrapping
	// runreq.ErrResumeUnrecoverable means the continuation can never run;
	// ErrContinuationsBusy means there is no capacity for it now, and the session
	// waits for the next trigger.
	Start(ctx context.Context, c Continuation) (done <-chan struct{}, err error)
	// Retire drops the continuation's intent, so that it is not owed again. The
	// run itself stays lost.
	Retire(ctx context.Context, c Continuation, cause error) error
}

// WorkspaceReadiness reports whether the workspace a run would start on can be
// reached; the error says why not. targetID empty is the session's default.
type WorkspaceReadiness func(ctx context.Context, botID, targetID string) error

// readinessBound limits one readiness check, which holds the session while it
// runs.
const readinessBound = 3 * time.Second

func (in *Inputs) workspaceReady(ctx context.Context, botID, targetID string) error {
	ctx, cancel := context.WithTimeout(ctx, readinessBound)
	defer cancel()
	return in.readiness(ctx, botID, targetID)
}

// admitContinuation starts the continuation the session is owed, if any, and
// retires the ones that can never run. started means its run exists; deferred
// means the session has to wait, so nothing behind the continuation may start.
// Both are false when the session owes nothing. The only error is
// sessionruntime.ErrManagerClosed: the process is stopping, and the next one
// takes the session over.
//
// A workspace that is not ready, a session another run holds, and any other
// failure that may clear, leave the intent in place and defer the whole
// session to the next trigger.
func (in *Inputs) admitContinuation(ctx context.Context, key sessionruntime.Key) (started, deferred bool, err error) {
	for ctx.Err() == nil {
		owed, found, err := in.continuations.Owed(ctx, key)
		switch {
		case errors.Is(err, runreq.ErrResumeUnrecoverable):
			if !in.retireContinuation(ctx, owed, err) {
				return false, true, nil
			}
			continue
		case err != nil:
			in.logger.WarnContext(ctx, "read owed continuation failed; session deferred",
				slog.String("session_id", key.SessionID), slog.Any("error", err))
			return false, true, nil
		case !found:
			return false, false, nil
		}
		if err := in.workspaceReady(ctx, key.BotID, owed.WorkspaceTargetID); err != nil {
			in.logger.InfoContext(ctx, "workspace not ready; continuation deferred",
				slog.String("session_id", key.SessionID), slog.Any("error", err))
			return false, true, nil
		}
		_, err = in.continuations.Start(ctx, owed)
		switch {
		case err == nil:
			return true, false, nil
		case errors.Is(err, sessionruntime.ErrManagerClosed):
			return false, true, err
		case errors.Is(err, ErrContinuationsBusy):
			in.logger.DebugContext(ctx, "no continuation slot free; session deferred",
				slog.String("session_id", key.SessionID))
			return false, true, nil
		case errors.Is(err, runreq.ErrResumeUnrecoverable):
			if !in.retireContinuation(ctx, owed, err) {
				return false, true, nil
			}
			continue
		}
		in.logger.WarnContext(ctx, "start continuation failed; session deferred",
			slog.String("session_id", key.SessionID), slog.String("run_id", owed.Run.RunID.String()), slog.Any("error", err))
		return false, true, nil
	}
	return false, true, nil
}

// retireContinuation retires an intent that can never run and reports whether
// that took effect; one that stays would be found again.
func (in *Inputs) retireContinuation(ctx context.Context, c Continuation, cause error) bool {
	if err := in.continuations.Retire(ctx, c, cause); err != nil {
		in.logger.WarnContext(ctx, "retire continuation failed; session deferred",
			slog.String("run_id", c.Run.RunID.String()), slog.Any("error", err))
		return false
	}
	in.logger.InfoContext(ctx, "interrupted session will not resume",
		slog.String("run_id", c.Run.RunID.String()), slog.Any("reason", cause))
	return true
}
