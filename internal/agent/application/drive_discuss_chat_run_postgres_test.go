package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// A message that names a runtime command in a discuss session is not passive
// input: the inbound path starts a chat turn for it, and the turn writes the
// message as its request row, with the event. These tests drive that turn
// through the application against the real ledger and history, then ask the
// discuss admission what is left. The session is an External Agent runtime's in
// the database; the turn itself runs on the scripted native model.

// chatTurn starts the chat turn the inbound path starts for an addressed
// message: its event is recorded without a row, and the turn writes the row.
func (p *discussProcess) chatTurn(t *testing.T, env discussEnv, from discussParticipant, externalID, text string) (eventID string, handle turn.RunHandle) {
	t.Helper()
	eventID = uuid.NewString()
	eventData, err := json.Marshal(map[string]any{"message_id": externalID, "mentions_me": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO bot_session_events (id, bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms)
		VALUES ($1, $2, $3, 'message', $4, $5, $6)
	`, eventID, env.botID, env.sessionID, eventData, externalID, time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert event %s: %v", externalID, err)
	}
	handle, err = p.svc.StartTurn(context.Background(), turn.StartTurnCommand{
		SchemaVersion:           1,
		TeamID:                  "team-test",
		Mode:                    turn.ModeChat,
		BotID:                   env.botID,
		ChatID:                  env.botID,
		ThreadID:                env.sessionID,
		RouteID:                 env.routeID,
		IdempotencyKey:          "chat:" + externalID,
		ExternalMessageID:       externalID,
		EventID:                 eventID,
		Query:                   text,
		UserVisibleText:         text,
		MentionsBot:             true,
		SourceChannelIdentityID: from.identityID,
		CurrentChannel:          discussTestPlatform,
		ReplyTarget:             discussTestTarget,
		ConversationType:        "group",
	})
	if err != nil {
		t.Fatalf("StartTurn(%s) error = %v", externalID, err)
	}
	return eventID, handle
}

// allRuns counts every run of the session, whatever started it.
func (e discussEnv) allRuns(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM session_runs WHERE session_id = $1`, e.sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requestRow is the history row a chat turn wrote for its request.
func (e discussEnv) requestRow(t *testing.T, eventID string) (turnID, runID string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `
		SELECT turn_id::text, run_id::text FROM bot_history_messages WHERE session_id = $1 AND event_id = $2 AND role = 'user'
	`, e.sessionID, eventID).Scan(&turnID, &runID); err != nil {
		t.Fatalf("the turn wrote no request row for event %s: %v", eventID, err)
	}
	return turnID, runID
}

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
			drainDriveHandle(t, handle)
			if _, runID := env.requestRow(t, eventID); runID != handle.RunID() {
				t.Fatalf("request row run = %q, want the turn's run %q", runID, handle.RunID())
			}
			settle()

			if got := env.allRuns(t); got != 1 {
				t.Fatalf("runs after the command turn = %d, want the turn alone", got)
			}
			if outcome, _ := proc.admit(t, env); outcome != DiscussNothingPending {
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
	drainDriveHandle(t, handle)
	settle()

	if outcome, _ := proc.admit(t, env); outcome != DiscussSkipped {
		t.Fatalf("check after the turn = %v, want skipped: the earlier input never addressed the bot", outcome)
	}
	if got := env.allRuns(t); got != 1 {
		t.Fatalf("runs = %d, want the command turn alone", got)
	}

	// Addressing the bot answers everything before it, the earlier input too.
	hw := env.write(t, passiveInput{from: alice, externalID: "m2", text: "@bot over to you", mentionsMe: true})
	outcome, done := proc.admit(t, env)
	if outcome != DiscussAdmitted {
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
		_, err := dbsqlc.New(env.pool).GetDiscussPendingHead(context.Background(), dbsqlc.GetDiscussPendingHeadParams{
			SessionID: db.ParseUUIDOrEmpty(env.sessionID), CoverPosition: pgtype.Int8{Valid: true},
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("pending head while the turn runs: error = %v, want none", err)
		}
		close(held.gate)
		drainDriveHandle(t, handle)
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
		drainDriveHandle(t, handle)
		settle()
		if got := env.allRuns(t); got != 1 {
			t.Fatalf("runs after a failed turn = %d, want the turn alone", got)
		}
		if outcome, _ := proc.admit(t, env); outcome != DiscussNothingPending {
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
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(first), "failed")

	_, handle := proc.chatTurn(t, env, alice, "cmd-1", "@bot /status")
	drainDriveHandle(t, handle)
	settle()

	// The failed batch is still the pending head: nothing but a newer input
	// admits it again, and the command's request row is not that input.
	if outcome, _ := proc.admit(t, env); outcome != DiscussSkipped {
		t.Fatalf("check after the command turn = %v, want skipped for the unchanged batch", outcome)
	}
	second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "second batch"})
	outcome, done = proc.admit(t, env)
	if outcome != DiscussAdmitted {
		t.Fatalf("outcome after a new input = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(second), "completed")
	if got := env.cover(t); got != second {
		t.Fatalf("cover = %d, want %d", got, second)
	}
}
