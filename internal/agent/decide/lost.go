package decide

import (
	"context"
	"errors"
	"log/slog"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runreq"
)

// cancelPendingRuntimeApprovals closes the residual approval window when a turn
// dies abnormally: any pending row for the session belonged to that turn (the
// pool's turn slot guarantees one turn per session), and its waiter is gone -
// left pending, the persisted card would stay actionable forever and a late
// approve would flip a row nobody executes.
func (d *Decider) CancelPendingApprovals(ctx context.Context, req runreq.ChatRequest, reason string) {
	if d.toolApproval == nil {
		return
	}
	cancelled, err := d.toolApproval.CancelPendingForSession(ctx, req.BotID, req.ThreadID, reason)
	if err != nil {
		d.logger.WarnContext(ctx, "cancel pending External Agent approvals failed",
			slog.String("bot_id", req.BotID),
			slog.String("session_id", req.ThreadID),
			slog.Any("error", err))
		return
	}
	if len(cancelled) > 0 {
		d.logger.InfoContext(ctx, "cancelled pending External Agent approvals with their turn",
			slog.String("session_id", req.ThreadID),
			slog.Int("count", len(cancelled)))
	}
}

// cancelLostRunDecisions closes the decisions a lost run left parked. Both
// kinds are attempted even when one fails: a lost run has no waiter left, and
// a stranded pending row would stay answerable.
func (d *Decider) CancelLostRunDecisions(ctx context.Context, botID, sessionID, runID string, fencingToken int64, reason string) error {
	var errs []error
	if canceller, ok := d.userInput.(interface {
		CancelPendingForRun(context.Context, string, string, string, int64, string) ([]userinput.Request, error)
	}); ok {
		_, err := canceller.CancelPendingForRun(ctx, botID, sessionID, runID, fencingToken, reason)
		errs = append(errs, err)
	}
	if d.toolApproval != nil {
		_, err := d.toolApproval.CancelPendingForRun(ctx, botID, sessionID, runID, fencingToken, reason)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
