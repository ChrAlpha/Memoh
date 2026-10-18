package pending

import (
	"context"
	"fmt"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

// GateTurn is the session queue's gate on a new turn. A turn whose message the
// queue already holds or has taken was delivered before, and is answered like
// any redelivery. A turn that would pass input queued ahead of it is busy: the
// input waits its turn in the queue, in order. The queue's own admission is the
// one command that neither waits nor is counted.
func (in *Inputs) GateTurn(ctx context.Context, cmd turn.StartTurnCommand) error {
	if cmd.NoDefer || strings.TrimSpace(cmd.ThreadID) == "" {
		return nil
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
