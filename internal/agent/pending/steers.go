package pending

import (
	"context"
	"errors"
	"log/slog"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// closeSteerQueueForRun rejects the terminal run's unapplied steers. A steer
// targets exactly one run; once that run is terminal it can never enter a
// model step, and leaving it accepted would show a dead item in the queue.
func (in *Inputs) closeSteerQueueForRun(ctx context.Context, terminal sessionruntime.TerminalRun) {
	if terminal.RunID == "" || terminal.BotID == "" || terminal.SessionID == "" {
		return
	}
	key := sessionruntime.Key{BotID: terminal.BotID, SessionID: terminal.SessionID}
	if err := in.runtime.CloseSteerRun(ctx, key, terminal.RunID); err != nil && !errors.Is(err, sessionruntime.ErrLiveQueueUnavailable) {
		in.logger.WarnContext(ctx, "close steer queue for terminal run failed",
			slog.String("run_id", terminal.RunID), slog.Any("error", err))
	}
}
