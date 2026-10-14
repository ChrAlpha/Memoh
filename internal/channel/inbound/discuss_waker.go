package inbound

import (
	"context"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
)

// discussWakeTimeout bounds one wake. The check it asks for reads a few rows
// and starts a run that outlives the call, so a wake that takes longer than
// this has lost its server.
const discussWakeTimeout = 30 * time.Second

// turnDiscussWaker tells the server that a discuss session took input. The
// server's admission decides whether anything is left to answer, so the waker
// carries no state and calling it twice is harmless.
type turnDiscussWaker struct {
	admitter turn.DiscussAdmitter
	logger   *slog.Logger
}

// NewDiscussWaker wakes a discuss session through the turn port: in the server
// process the admission itself, in the channel process the turn RPC.
func NewDiscussWaker(admitter turn.DiscussAdmitter, logger *slog.Logger) DiscussWaker {
	if logger == nil {
		logger = slog.Default()
	}
	return &turnDiscussWaker{admitter: admitter, logger: logger.With(slog.String("component", "discuss_waker"))}
}

// Wake returns at once. The inbound request that committed the input is over
// when the check runs, so the check does not inherit its cancellation; the
// sweep answers what a failed wake leaves behind.
func (w *turnDiscussWaker) Wake(ctx context.Context, teamID, botID, sessionID string) {
	wakeCtx := context.WithoutCancel(ctx)
	go func() {
		wakeCtx, cancel := context.WithTimeout(wakeCtx, discussWakeTimeout)
		defer cancel()
		if _, err := w.admitter.AdmitDiscuss(wakeCtx, turn.DiscussAdmitCommand{TeamID: teamID, BotID: botID, ThreadID: sessionID}); err != nil {
			w.logger.WarnContext(wakeCtx, "wake discuss session failed; the sweep retries",
				slog.String("bot_id", botID), slog.String("session_id", sessionID), slog.Any("error", err))
		}
	}()
}
