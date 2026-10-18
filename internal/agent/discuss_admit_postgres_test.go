package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/team"
)

// admitCommand is the check the channel side asks for, over the env's session.
func (e discussEnv) admitCommand() turn.DiscussAdmitCommand {
	return turn.DiscussAdmitCommand{TeamID: team.DefaultTeamID, BotID: e.botID, ThreadID: e.sessionID}
}

// The port gives each of the four answers of the admission its own outcome.
func TestAdmitDiscussPortReportsEveryOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	held := &discussStep{reached: make(chan struct{}), gate: make(chan struct{})}
	proc := newDiscussProcessWith(t, env, []discussOption{discussTeam(team.DefaultTeamID)}, held)

	got, err := proc.svc.AdmitDiscuss(ctx, env.admitCommand())
	if err != nil || got != turn.DiscussNothingPending {
		t.Fatalf("with nothing recorded: outcome = %q, error = %v, want nothing pending", got, err)
	}
	first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "first"})
	got, err = proc.svc.AdmitDiscuss(ctx, env.admitCommand())
	if err != nil || got != turn.DiscussAdmitted {
		t.Fatalf("with input recorded: outcome = %q, error = %v, want admitted", got, err)
	}
	waitReached(t, held)
	env.write(t, passiveInput{from: alice, externalID: "m2", text: "second"})
	got, err = proc.svc.AdmitDiscuss(ctx, env.admitCommand())
	if err != nil || got != turn.DiscussBusy {
		t.Fatalf("while the session is held: outcome = %q, error = %v, want busy", got, err)
	}
	close(held.gate)
	env.waitRunState(t, env.invocation(first), "completed")

	// A session whose runs start from an addressed message has nothing to admit.
	other := newDiscussEnv(t, "group")
	if _, err := other.pool.Exec(ctx, `UPDATE bot_sessions SET reply_delivery = 'stream', trigger_scope = 'addressed_message' WHERE id = $1`, other.sessionID); err != nil {
		t.Fatal(err)
	}
	otherProc := newDiscussProcessWith(t, other, []discussOption{discussTeam(team.DefaultTeamID)})
	other.write(t, passiveInput{from: other.participant(t, false), externalID: "m1", text: "a message"})
	got, err = otherProc.svc.AdmitDiscuss(ctx, other.admitCommand())
	if err != nil || got != turn.DiscussSkipped {
		t.Fatalf("session without the trigger scope: outcome = %q, error = %v, want skipped", got, err)
	}
}

// A caller that is gone, as the RPC of a wake is once it has answered, does not
// take down the run it started.
func TestAdmitDiscussRunOutlivesTheCallerContext(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	held := &discussStep{reached: make(chan struct{}), gate: make(chan struct{}), send: "still here"}
	proc := newDiscussProcessWith(t, env, []discussOption{discussTeam(team.DefaultTeamID)}, held, &discussStep{})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "are you there?"})

	ctx, cancel := context.WithCancel(context.Background())
	got, err := proc.svc.AdmitDiscuss(ctx, env.admitCommand())
	if err != nil || got != turn.DiscussAdmitted {
		t.Fatalf("outcome = %q, error = %v, want admitted", got, err)
	}
	cancel()
	waitReached(t, held)
	close(held.gate)
	env.waitRunState(t, env.invocation(hw), "completed")
	if sent := proc.sender.delivered(); len(sent) != 1 {
		t.Fatalf("delivered = %+v, want the run's send", sent)
	}
}

// The port serves the team the service is bound to and needs the session named.
func TestAdmitDiscussPortRefusesWhatItCannotServe(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc := newDiscussProcessWith(t, env, []discussOption{discussTeam(team.DefaultTeamID)})
	ctx := context.Background()

	other := env.admitCommand()
	other.TeamID = "00000000-0000-0000-0000-0000000000ff"
	if _, err := proc.svc.AdmitDiscuss(ctx, other); !errors.Is(err, turn.ErrTeamNotServed) {
		t.Fatalf("another team: error = %v, want ErrTeamNotServed", err)
	}
	noTeam := env.admitCommand()
	noTeam.TeamID = ""
	if _, err := proc.svc.AdmitDiscuss(ctx, noTeam); !errors.Is(err, turn.ErrTeamNotServed) {
		t.Fatalf("no team: error = %v, want ErrTeamNotServed", err)
	}
	noSession := env.admitCommand()
	noSession.ThreadID = ""
	if _, err := proc.svc.AdmitDiscuss(ctx, noSession); err == nil {
		t.Fatal("a check without a session was accepted")
	}
}
