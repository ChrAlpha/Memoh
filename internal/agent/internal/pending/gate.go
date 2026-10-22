package pending

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// GateTurn is the session queue's gate on a new turn. A turn whose message the
// queue already holds or has taken was delivered before, and is answered like
// any redelivery. A turn that would pass input queued ahead of it is busy: the
// input waits its turn in the queue, in order. The queue's own admission is the
// one command that neither waits nor is counted; a platform message the channel
// process takes out of the queue is owed to the session like the rest, and
// meets the order the session's owed work is started in.
func (in *Inputs) GateTurn(ctx context.Context, cmd turn.StartTurnCommand) error {
	if strings.TrimSpace(cmd.ThreadID) == "" {
		return nil
	}
	switch cmd.Origin {
	case turn.OriginPending:
		return nil
	case turn.OriginPlatformQueue:
		return in.gateOwedStart(ctx, cmd)
	case turn.OriginDirect:
	}
	gate, err := in.queue.Gate(ctx, sessionruntime.Key{BotID: cmd.BotID, SessionID: cmd.ThreadID}, cmd.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("check session queue: %w", err)
	}
	switch {
	case gate.Seen:
		return fmt.Errorf("%w: %s", turn.ErrDuplicateTurn, cmd.IdempotencyKey)
	case gate.Waiting:
		return fmt.Errorf("%w: thread %s has queued input", turn.ErrSessionBusy, cmd.ThreadID)
	}
	return nil
}

// gateOwedStart lets a start of owed work through when nothing is owed ahead of
// it. A continuation of an interrupted run is owed first, and is started here
// when its turn has come, so the message that asked is busy either way; the
// workspace the message would run on has to be reachable. A busy answer leaves
// the message in the queue, which tries it again.
func (in *Inputs) gateOwedStart(ctx context.Context, cmd turn.StartTurnCommand) error {
	unlock := in.sessions.lock(cmd.BotID + ":" + cmd.ThreadID)
	defer unlock()
	key := sessionruntime.Key{BotID: cmd.BotID, SessionID: cmd.ThreadID}
	started, deferred, err := in.admitContinuation(ctx, key)
	switch {
	case errors.Is(err, sessionruntime.ErrManagerClosed):
		return err
	case started || deferred:
		return fmt.Errorf("%w: thread %s is owed a continuation", turn.ErrSessionBusy, cmd.ThreadID)
	}
	if err := in.workspaceReady(ctx, cmd.BotID, cmd.WorkspaceTargetID); err != nil {
		in.logger.InfoContext(ctx, "workspace not ready; queued start deferred",
			slog.String("session_id", cmd.ThreadID), slog.Any("error", err))
		return fmt.Errorf("%w: workspace of thread %s is not ready", turn.ErrSessionBusy, cmd.ThreadID)
	}
	return nil
}
