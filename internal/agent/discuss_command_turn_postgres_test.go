package agent

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// settle gives the checks a run's end starts on its own time to act.
func settle() { time.Sleep(500 * time.Millisecond) }

// The message that named the command is answered by the turn it started, once.
// The discuss admission, which a turn's end asks about the session, finds
// nothing left for it.
func TestDiscussCommandTurnAnswersItsMessageOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	for _, runtime := range []string{"model", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			env := newDiscussEnv(t, "group")
			if _, err := env.pool.Exec(context.Background(), `UPDATE bot_sessions SET runtime_type = $2 WHERE id = $1`, env.sessionID, runtime); err != nil {
				t.Fatal(err)
			}
			proc := newDiscussProcess(t, env, &discussStep{text: "all good"})
			alice := env.participant(t, false)

			eventID, handle := proc.chatTurn(t, env, alice, "cmd-1", "@bot /status")
			drainTurn(t, handle)
			if _, runID := env.requestRow(t, eventID); runID != handle.RunID() {
				t.Fatalf("request row run = %q, want the turn's run %q", runID, handle.RunID())
			}
			settle()

			if got := env.allRuns(t); got != 1 {
				t.Fatalf("runs after the command turn = %d, want the turn alone", got)
			}
			if outcome, _ := proc.admit(t, env); outcome != pending.DiscussNothingPending {
				t.Fatalf("check after the turn = %v, want nothing pending", outcome)
			}
			if calls := len(proc.provider.recorded()); calls != 1 {
				t.Fatalf("model calls = %d, want the turn's one", calls)
			}
		})
	}
}

// Input that was passive stays pending when a command turn answers a later
// message, and the answered message does not count as asking for it: an
// External Agent runtime stays idle until someone addresses the bot again.
func TestDiscussCommandTurnLeavesEarlierInputPending(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	if _, err := env.pool.Exec(context.Background(), `UPDATE bot_sessions SET runtime_type = 'codex' WHERE id = $1`, env.sessionID); err != nil {
		t.Fatal(err)
	}
	proc := newDiscussProcess(t, env, &discussStep{text: "all good"}, &discussStep{})
	alice := env.participant(t, false)
	env.write(t, passiveInput{from: alice, externalID: "m1", text: "talking among ourselves"})

	_, handle := proc.chatTurn(t, env, alice, "cmd-1", "@bot /status")
	drainTurn(t, handle)
	settle()

	if outcome, _ := proc.admit(t, env); outcome != pending.DiscussSkipped {
		t.Fatalf("check after the turn = %v, want skipped: the earlier input never addressed the bot", outcome)
	}
	if got := env.allRuns(t); got != 1 {
		t.Fatalf("runs = %d, want the command turn alone", got)
	}

	// Addressing the bot answers everything before it, the earlier input too.
	hw := env.write(t, passiveInput{from: alice, externalID: "m2", text: "@bot over to you", mentionsMe: true})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome after an addressed input = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(hw), "completed")
	if got := env.cover(t); got != hw {
		t.Fatalf("cover = %d, want %d", got, hw)
	}
}

// The rule does not depend on how the turn ended, nor on whether it has ended:
// a message a turn took as its request is not discuss input while the turn
// runs, after it fails, or after it is stopped.
func TestDiscussCommandTurnRequestIsNeverPending(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("running", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		// The turn's first step is committed with its request row while the
		// turn is still on its second model call.
		held := &discussStep{reached: make(chan struct{}), gate: make(chan struct{}), text: "all good"}
		proc := newDiscussProcess(t, env, &discussStep{send: "on it"}, held)
		alice := env.participant(t, false)

		eventID, handle := proc.chatTurn(t, env, alice, "cmd-1", "@bot /status")
		waitReached(t, held)
		env.requestRow(t, eventID)
		pendingInputs, err := dbsqlc.New(env.pool).ListDiscussPendingInputs(context.Background(), dbsqlc.ListDiscussPendingInputsParams{
			SessionID: db.ParseUUIDOrEmpty(env.sessionID), CoverPosition: pgtype.Int8{Valid: true},
		})
		if err != nil || len(pendingInputs) != 0 {
			t.Fatalf("pending input while the turn runs: %d rows, error = %v, want none", len(pendingInputs), err)
		}
		close(held.gate)
		drainTurn(t, handle)
		settle()
		if got := env.allRuns(t); got != 1 {
			t.Fatalf("runs = %d, want the turn alone", got)
		}
	})
	t.Run("failed", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		proc := newDiscussProcess(t, env, &discussStep{fail: true})
		alice := env.participant(t, false)

		_, handle := proc.chatTurn(t, env, alice, "cmd-1", "@bot /status")
		drainTurn(t, handle)
		settle()
		if got := env.allRuns(t); got != 1 {
			t.Fatalf("runs after a failed turn = %d, want the turn alone", got)
		}
		if outcome, _ := proc.admit(t, env); outcome != pending.DiscussNothingPending {
			t.Fatalf("check after a failed turn = %v, want nothing pending", outcome)
		}
	})
}

// A discuss run that failed, was stopped or was lost answered nothing, so its
// batch stays pending whether or not a command turn ran in between.
func TestDiscussFailedBatchStaysPendingAcrossCommandTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc := newDiscussProcess(t, env, &discussStep{fail: true}, &discussStep{text: "all good"}, &discussStep{})
	alice := env.participant(t, false)
	first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "first batch"})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(first), "failed")

	_, handle := proc.chatTurn(t, env, alice, "cmd-1", "@bot /status")
	drainTurn(t, handle)
	settle()

	// The failed batch is still the pending head: nothing but a newer input
	// admits it again, and the command's request row is not that input.
	if outcome, _ := proc.admit(t, env); outcome != pending.DiscussSkipped {
		t.Fatalf("check after the command turn = %v, want skipped for the unchanged batch", outcome)
	}
	second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "second batch"})
	outcome, done = proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome after a new input = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(second), "completed")
	if got := env.cover(t); got != second {
		t.Fatalf("cover = %d, want %d", got, second)
	}
}
