package turn

import (
	"context"
	"fmt"
)

// DiscussAdmitCommand names the discuss session whose external input is to be
// answered. TeamID must match the runtime instance, as for StopCommand.
type DiscussAdmitCommand struct{ TeamID, BotID, ThreadID string }

// DiscussOutcome is what one admission check of a discuss session did.
type DiscussOutcome string

const (
	// DiscussAdmitted means a run now answers the pending external input.
	DiscussAdmitted DiscussOutcome = "admitted"
	// DiscussNothingPending means every external input is already covered.
	DiscussNothingPending DiscussOutcome = "nothing_pending"
	// DiscussBusy means another run holds the session; that run's end checks
	// the session again.
	DiscussBusy DiscussOutcome = "busy"
	// DiscussSkipped means the pending input is not answered now: the session
	// does not answer uncovered events, its batch was already admitted once, or
	// an External Agent runtime was not addressed.
	DiscussSkipped DiscussOutcome = "skipped"
)

// Valid reports whether the outcome is one of the four a check can end in.
func (o DiscussOutcome) Valid() bool {
	switch o {
	case DiscussAdmitted, DiscussNothingPending, DiscussBusy, DiscussSkipped:
		return true
	default:
		return false
	}
}

// ParseDiscussOutcome reads an outcome off the wire and refuses a value no
// server of this version sends.
func ParseDiscussOutcome(value string) (DiscussOutcome, error) {
	outcome := DiscussOutcome(value)
	if !outcome.Valid() {
		return "", fmt.Errorf("turn: unknown discuss outcome %q", value)
	}
	return outcome, nil
}

// DiscussAdmitter checks a discuss session and answers its uncovered external
// input. Any caller may call it at any time and as often as it likes: the
// server decides whether anything is left to answer. Kept separate from
// Service so that only the process that runs discuss turns implements it.
type DiscussAdmitter interface {
	AdmitDiscuss(ctx context.Context, cmd DiscussAdmitCommand) (DiscussOutcome, error)
}
