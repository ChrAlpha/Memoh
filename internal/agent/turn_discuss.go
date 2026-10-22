package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/felinics/memoh/internal/agent/internal/pending"
	"github.com/felinics/memoh/internal/agent/turn"
)

var _ turn.DiscussAdmitter = (*Service)(nil)

// AdmitDiscuss is the discuss admission behind the turn port. It is what the
// channel side calls, in the same process or over the turn RPC, to say that a
// discuss session took external input; the check itself, and the run it
// admits, are the server's either way.
//
// The caller's context ends with its request, which is no reason to end a run
// it caused, so the check runs on a context that outlives it.
func (s *Service) AdmitDiscuss(ctx context.Context, cmd turn.DiscussAdmitCommand) (turn.DiscussOutcome, error) {
	if cmd.TeamID == "" || (s.allowedTeam != "" && cmd.TeamID != s.allowedTeam) {
		return "", turn.ErrTeamNotServed
	}
	if cmd.BotID == "" || cmd.ThreadID == "" {
		return "", errors.New("bot and thread are required")
	}
	admission, err := s.pending.CheckAndAdmitDiscuss(context.WithoutCancel(ctx), cmd.BotID, cmd.ThreadID)
	if err != nil {
		return "", err
	}
	return discussOutcome(admission)
}

func discussOutcome(admission pending.DiscussAdmission) (turn.DiscussOutcome, error) {
	switch admission {
	case pending.DiscussAdmitted:
		return turn.DiscussAdmitted, nil
	case pending.DiscussNothingPending:
		return turn.DiscussNothingPending, nil
	case pending.DiscussBusy:
		return turn.DiscussBusy, nil
	case pending.DiscussSkipped:
		return turn.DiscussSkipped, nil
	default:
		return "", fmt.Errorf("unknown discuss admission %d", admission)
	}
}
