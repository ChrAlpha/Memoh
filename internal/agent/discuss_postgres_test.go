package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/internal/admit"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/pending"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/bots"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/logger"
	"github.com/felinics/memoh/internal/testutil/routecredentials"
)

// discussAdmitter replaces the session runtime of admission with what wrap
// builds over the process's own manager.
func discussAdmitter(wrap func(*sessionruntime.Manager) runreq.TurnAdmitter) discussOption {
	return func(s *discussSetup) {
		s.fixture = append(s.fixture, withFixtureOptions(withTurnAdmitter(wrap(s.manager))))
	}
}

// A first external input is answered by one run with nothing before it. The
// run speaks only through the send tool, into the route's conversation, and
// acts for the participant who wrote the input; once it completes the input is
// covered.
func TestDiscussAnswersPendingInputThroughSend(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, true)
	proc := newDiscussProcess(t, env, &discussStep{send: "The freeze is Friday."})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "is the freeze on this week?"})

	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)

	run := env.waitRunState(t, env.invocation(hw), "completed")
	if run.coverPosition != hw {
		t.Fatalf("run cover_position = %d, want %d", run.coverPosition, hw)
	}
	if got := env.cover(t); got != hw {
		t.Fatalf("cover position = %d, want %d", got, hw)
	}
	sent := proc.sender.delivered()
	if len(sent) != 1 || sent[0].platform != discussTestPlatform || sent[0].target != discussTestTarget || !strings.Contains(sent[0].text, "The freeze is Friday.") {
		t.Fatalf("delivered = %+v, want one send into the route's conversation", sent)
	}
	requests := proc.provider.recorded()
	if len(requests) == 0 {
		t.Fatal("the run never reached the model")
	}
	first := requestText(t, requests[0])
	if !strings.Contains(first, "is the freeze on this week?") {
		t.Fatalf("the model was not shown the input: %s", first)
	}
	// Explicit delivery: the model is told that not calling send is silence.
	if !strings.Contains(first, "if you do not call it, you stay silent") {
		t.Fatalf("the run was not told it speaks through send: %s", first)
	}

	// The credentials name the participant who wrote the input and the route
	// it came in on; the session acts for the bot's owner. They are recorded
	// for a later resume like any other run's, and are what the inbound path
	// signs for the same participant: its tests hold it to the same scenario.
	var resume admit.ResumeContext
	if err := json.Unmarshal(run.input["resume"], &resume); err != nil {
		t.Fatalf("resume context: %v (%s)", err, run.input["resume"])
	}
	wantChat, wantSession := routecredentials.Scenario{
		BotID: env.botID, RouteID: env.routeID, ChannelIdentityID: alice.identityID,
		ParticipantUserID: alice.userID, BotOwnerUserID: env.ownerID,
	}.Want()
	if !reflect.DeepEqual(resume.ChatClaims, wantChat) || !reflect.DeepEqual(resume.TokenClaims, wantSession) {
		t.Fatalf("run credentials\nchat    %v\nsession %v\nwant\nchat    %v\nsession %v", resume.ChatClaims, resume.TokenClaims, wantChat, wantSession)
	}
	if resume.ChannelIdentityID != alice.identityID || resume.ReplyTarget != discussTestTarget || resume.CurrentChannel != discussTestPlatform {
		t.Fatalf("resume context = %+v", resume)
	}

	if outcome, _ := proc.admit(t, env); outcome != pending.DiscussNothingPending {
		t.Fatalf("check after completion = %v, want nothing pending", outcome)
	}
}

// Which sessions are answered this way is the session's trigger scope. A
// session named discuss whose runs start from an addressed message is not, and
// one named chat whose runs answer every uncovered event is.
func TestDiscussAdmissionReadsTriggerScope(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("addressed message", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		if _, err := env.pool.Exec(context.Background(), `
			UPDATE bot_sessions SET reply_delivery = 'stream', trigger_scope = 'addressed_message' WHERE id = $1
		`, env.sessionID); err != nil {
			t.Fatal(err)
		}
		proc := newDiscussProcess(t, env)
		env.write(t, passiveInput{from: env.participant(t, false), externalID: "m1", text: "a message"})
		if outcome, _ := proc.admit(t, env); outcome != pending.DiscussSkipped {
			t.Fatalf("outcome = %v, want skipped", outcome)
		}
		if got := len(env.runs(t)); got != 0 {
			t.Fatalf("runs = %d, want 0", got)
		}
	})
	t.Run("uncovered events", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		if _, err := env.pool.Exec(context.Background(), `
			UPDATE bot_sessions SET type = 'chat', session_mode = 'chat' WHERE id = $1
		`, env.sessionID); err != nil {
			t.Fatal(err)
		}
		proc := newDiscussProcess(t, env)
		hw := env.write(t, passiveInput{from: env.participant(t, false), externalID: "m1", text: "a message"})
		outcome, done := proc.admit(t, env)
		if outcome != pending.DiscussAdmitted {
			t.Fatalf("outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(hw), "completed")
	})
}

// A participant bound to more than one account names no account, as on the
// inbound path: the chat token names the channel identity alone.
func TestDiscussAmbiguousParticipantNamesNoAccount(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	bob := env.participant(t, true)
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO user_channel_identity_bindings (user_id, channel_identity_id) VALUES ($1, $2)
	`, env.ownerID, bob.identityID); err != nil {
		t.Fatal(err)
	}
	proc := newDiscussProcess(t, env)
	hw := env.write(t, passiveInput{from: bob, externalID: "m1", text: "who am I"})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	run := env.waitRunState(t, env.invocation(hw), "completed")
	var resume admit.ResumeContext
	if err := json.Unmarshal(run.input["resume"], &resume); err != nil {
		t.Fatal(err)
	}
	if resume.UserID != "" || resume.ChatClaims["user_id"] != bob.identityID {
		t.Fatalf("ambiguous participant: user_id = %q, chat claims = %v, want no account", resume.UserID, resume.ChatClaims)
	}
}

// Only a completed run covers its batch. After the batch's run is aborted,
// failed or lost, the same batch is not admitted again; a newer input is, and
// its run answers both.
func TestDiscussUnchangedBatchIsNotAdmittedAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	for _, terminal := range []string{"aborted", "failed", "lost"} {
		t.Run(terminal, func(t *testing.T) {
			env := newDiscussEnv(t, "group")
			alice := env.participant(t, false)
			proc := newDiscussProcess(t, env)
			first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "first batch"})
			outcome, done := proc.admit(t, env)
			if outcome != pending.DiscussAdmitted {
				t.Fatalf("first outcome = %v, want admitted", outcome)
			}
			waitDiscussRun(t, done)
			env.waitRunState(t, env.invocation(first), "completed")

			if _, err := env.pool.Exec(context.Background(), `UPDATE session_runs SET state = $2 WHERE session_id = $1 AND invocation_id = $3`, env.sessionID, terminal, env.invocation(first)); err != nil {
				t.Fatal(err)
			}
			if got := env.cover(t); got != 0 {
				t.Fatalf("cover after %s = %d, want 0", terminal, got)
			}
			for range 2 {
				if outcome, _ := proc.admit(t, env); outcome != pending.DiscussSkipped {
					t.Fatalf("unchanged batch after %s: outcome = %v, want skipped", terminal, outcome)
				}
			}
			if got := len(env.runs(t)); got != 1 {
				t.Fatalf("runs after the unchanged batch = %d, want 1", got)
			}

			second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "second batch"})
			outcome, done = proc.admit(t, env)
			if outcome != pending.DiscussAdmitted {
				t.Fatalf("after a new input: outcome = %v, want admitted", outcome)
			}
			waitDiscussRun(t, done)
			env.waitRunState(t, env.invocation(second), "completed")
			if got := env.cover(t); got != second {
				t.Fatalf("cover = %d, want %d", got, second)
			}
			last := requestText(t, proc.provider.recorded()[len(proc.provider.recorded())-1])
			if !strings.Contains(last, "first batch") || !strings.Contains(last, "second batch") {
				t.Fatalf("the retry did not see both batches: %s", last)
			}
		})
	}
}

// Two processes checking one session at once admit one round between them:
// the PostgreSQL ledger and its fence arbitrate, not process memory.
func TestDiscussTwoManagersAdmitOneRound(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	procs := []*discussProcess{newDiscussProcess(t, env), newDiscussProcess(t, env)}
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "race"})

	const perProcess = 3
	type result struct {
		outcome pending.DiscussAdmission
		done    <-chan struct{}
		err     error
	}
	results := make([]result, len(procs)*perProcess)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		proc := procs[i%len(procs)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := &results[i]
			r.outcome, r.done, r.err = proc.svc.pending.AdmitDiscussRun(context.Background(), env.botID, env.sessionID)
		}()
	}
	close(start)
	wg.Wait()

	admitted := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
		if r.outcome == pending.DiscussAdmitted {
			admitted++
			waitDiscussRun(t, r.done)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted = %d, want 1", admitted)
	}
	env.waitRunState(t, env.invocation(hw), "completed")
	if got := len(env.runs(t)); got != 1 {
		t.Fatalf("discuss runs = %d, want 1", got)
	}
	calls := len(procs[0].provider.recorded()) + len(procs[1].provider.recorded())
	if calls != 1 {
		t.Fatalf("model calls = %d, want 1", calls)
	}
}

// An External Agent runtime in a group runs only once the pending input
// addresses the bot. Unaddressed input writes no run and stays pending; the
// first addressed input answers everything before it. A private conversation
// always addresses the bot.
func TestDiscussExternalRuntimeRunsOnlyWhenAddressed(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("group", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		alice := env.participant(t, false)
		env.setRuntime(t, "codex")
		proc := newDiscussProcess(t, env)
		env.write(t, passiveInput{from: alice, externalID: "m1", text: "not for the bot"})
		if outcome, _ := proc.admit(t, env); outcome != pending.DiscussSkipped {
			t.Fatalf("unaddressed outcome = %v, want skipped", outcome)
		}
		if got := len(env.runs(t)); got != 0 {
			t.Fatalf("runs for unaddressed input = %d, want 0", got)
		}
		hw := env.write(t, passiveInput{from: alice, externalID: "m2", text: "@bot over to you", mentionsMe: true})
		outcome, done := proc.admit(t, env)
		if outcome != pending.DiscussAdmitted {
			t.Fatalf("addressed outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
		if _, ok := env.runs(t)[env.invocation(hw)]; !ok {
			t.Fatalf("runs = %v, want the addressed batch's run", env.runs(t))
		}
	})
	t.Run("private", func(t *testing.T) {
		env := newDiscussEnv(t, "private")
		alice := env.participant(t, false)
		env.setRuntime(t, "codex")
		proc := newDiscussProcess(t, env)
		env.write(t, passiveInput{from: alice, externalID: "m1", text: "a direct message"})
		outcome, done := proc.admit(t, env)
		if outcome != pending.DiscussAdmitted {
			t.Fatalf("private outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
	})
}

// A message from a sender refused at admission is neither pending input nor
// part of the model's context.
func TestDiscussRefusedInputIsNotAnsweredOrShown(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	mallory := env.participant(t, false)
	proc := newDiscussProcess(t, env)

	env.write(t, passiveInput{from: mallory, externalID: "r0", text: "refused before anything", refused: messagepkg.AdmissionRejectedACL})
	if outcome, _ := proc.admit(t, env); outcome != pending.DiscussNothingPending {
		t.Fatalf("only refused input: outcome = %v, want nothing pending", outcome)
	}

	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "an admitted question"})
	env.write(t, passiveInput{from: mallory, externalID: "r1", text: "refused after it", refused: messagepkg.AdmissionRejectedACL})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	run := env.waitRunState(t, env.invocation(hw), "completed")
	if run.coverPosition != hw {
		t.Fatalf("cover_position = %d, want the admitted row's %d", run.coverPosition, hw)
	}
	text := requestText(t, proc.provider.recorded()[0])
	if !strings.Contains(text, "an admitted question") {
		t.Fatalf("the admitted input is missing: %s", text)
	}
	if strings.Contains(text, "refused before anything") || strings.Contains(text, "refused after it") {
		t.Fatalf("refused input reached the model: %s", text)
	}
}

// A discuss run commits each step as it goes, and a run that fails leaves a
// failure row against the input it was answering.
func TestDiscussCommitsStepsAndRecordsFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	t.Run("steps", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		alice := env.participant(t, false)
		second := &discussStep{reached: make(chan struct{}), gate: make(chan struct{})}
		proc := newDiscussProcess(t, env, &discussStep{send: "on it"}, second)
		hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "please check"})
		outcome, done := proc.admit(t, env)
		if outcome != pending.DiscussAdmitted {
			t.Fatalf("outcome = %v, want admitted", outcome)
		}
		waitReached(t, second)
		// The send step is in history while the run is still on its next call.
		var toolRows int
		if err := env.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND role = 'tool'
		`, env.sessionID).Scan(&toolRows); err != nil {
			t.Fatal(err)
		}
		if run := env.runs(t)[env.invocation(hw)]; run.state != "running" {
			t.Fatalf("run state while held = %q, want running", run.state)
		}
		if toolRows != 1 {
			t.Fatalf("tool rows while the run is still going = %d, want the committed send step", toolRows)
		}
		close(second.gate)
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(hw), "completed")
	})
	t.Run("failure", func(t *testing.T) {
		env := newDiscussEnv(t, "group")
		alice := env.participant(t, false)
		proc := newDiscussProcess(t, env, &discussStep{fail: true})
		hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "please check"})
		outcome, done := proc.admit(t, env)
		if outcome != pending.DiscussAdmitted {
			t.Fatalf("outcome = %v, want admitted", outcome)
		}
		waitDiscussRun(t, done)
		env.waitRunState(t, env.invocation(hw), "failed")
		if got := env.cover(t); got != 0 {
			t.Fatalf("cover after a failure = %d, want 0", got)
		}
		var users, failures int
		if err := env.pool.QueryRow(context.Background(), `
			SELECT count(*) FILTER (WHERE role = 'user'),
			       count(*) FILTER (WHERE COALESCE(metadata->>$2, '') <> '')
			FROM bot_history_messages WHERE session_id = $1
		`, env.sessionID, messagepkg.HistoryErrorCodeMetadataKey).Scan(&users, &failures); err != nil {
			t.Fatal(err)
		}
		if users != 1 || failures != 1 {
			t.Fatalf("history after failure: user rows = %d, failure rows = %d, want 1 and 1", users, failures)
		}
	})
}

// A discuss batch runs through the driver like every other entry, so it has a
// turn span of its own that says how the batch ended.
func TestDiscussRunHasOneTurnSpan(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	cases := []struct {
		name    string
		step    *discussStep
		state   string
		outcome string
		status  codes.Code
	}{
		{name: "completed", step: &discussStep{send: "The freeze is Friday."}, state: "completed", outcome: "completed", status: codes.Unset},
		{name: "failed", step: &discussStep{fail: true}, state: "failed", outcome: "errored", status: codes.Error},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := recordTurnSpans(t)
			env := newDiscussEnv(t, "group")
			alice := env.participant(t, true)
			proc := newDiscussProcess(t, env, tc.step)
			hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "is the freeze on this week?"})

			outcome, done := proc.admit(t, env)
			if outcome != pending.DiscussAdmitted {
				t.Fatalf("outcome = %v, want admitted", outcome)
			}
			waitDiscussRun(t, done)
			env.waitRunState(t, env.invocation(hw), tc.state)

			span := turnSpan(t, recorder)
			var spans int
			for _, ended := range recorder.Ended() {
				if ended.Name() == "agent.turn" {
					spans++
				}
			}
			if spans != 1 {
				t.Fatalf("agent.turn spans = %d, want exactly one", spans)
			}
			if got := spanAttr(span, "agent.turn.outcome").AsString(); got != tc.outcome {
				t.Errorf("outcome = %q, want %q", got, tc.outcome)
			}
			if got := span.Status().Code; got != tc.status {
				t.Errorf("status = %v, want %v", got, tc.status)
			}
		})
	}
}

// A discuss run interrupted by a shutdown continues as a resume run that
// carries the interrupted run's cover position, so the batch is covered once
// the continuation completes.
func TestDiscussResumeKeepsCoverPosition(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	ctx := context.Background()
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{send: "starting"})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "long task"})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	interrupted := env.waitRunState(t, env.invocation(hw), "completed")
	// The run was cut off by a shutdown instead.
	if _, err := env.pool.Exec(ctx, `
		UPDATE session_runs SET state = 'lost', error_code = 'session_runtime.interrupted' WHERE run_id = $1
	`, interrupted.runID); err != nil {
		t.Fatal(err)
	}
	if got := env.cover(t); got != 0 {
		t.Fatalf("cover with the run interrupted = %d, want 0", got)
	}

	rows, err := dbsqlc.New(env.pool).ListInterruptedSessionRuns(ctx, db.ParseUUIDOrEmpty(uuid.Nil.String()))
	if err != nil {
		t.Fatal(err)
	}
	var row dbsqlc.SessionRun
	for _, candidate := range rows {
		if candidate.RunID.String() == interrupted.runID {
			row = candidate
		}
	}
	if !row.RunID.Valid {
		t.Fatalf("the interrupted run is not listed for resume")
	}
	// The process that resumes it is the one the server restarted into: it
	// serves the run's team and knows the owner's account and permissions.
	proc = newDiscussProcessWith(t, env, []discussOption{
		discussTeam(row.TeamID.String()),
		discussDeps(func(d *Deps) {
			d.Accounts = accounts.NewService(nil, titleModelAccountStore{account: dbstore.AccountRecord{ID: env.ownerID, IsActive: true}})
			d.BotPermissions = &agenttest.PermissionChecker{Values: map[string]bool{env.botID + ":" + env.ownerID + ":" + bots.PermissionChat: true}}
		}),
	})
	resumed, err := startContinuation(ctx, proc.svc, row)
	if err != nil {
		t.Fatalf("resumeInterruptedSession() error = %v", err)
	}
	waitDiscussRun(t, resumed)

	var state string
	var cover int64
	if err := env.pool.QueryRow(ctx, `
		SELECT state, COALESCE(discuss_cover_position, 0) FROM session_runs WHERE invocation_id = $1
	`, "resume:"+interrupted.runID).Scan(&state, &cover); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || cover != hw {
		t.Fatalf("resume run state = %q cover_position = %d, want completed and %d", state, cover, hw)
	}
	// The continuation still sees the input it was answering.
	requests := proc.provider.recorded()
	if last := requestText(t, requests[len(requests)-1]); !strings.Contains(last, "long task") || !strings.Contains(last, "gracefully shut down") {
		t.Fatalf("the resumed run lost its input or instruction: %s", last)
	}
	if got := env.cover(t); got != hw {
		t.Fatalf("cover after the resume = %d, want %d", got, hw)
	}
	if outcome, _ := proc.admit(t, env); outcome != pending.DiscussNothingPending {
		t.Fatalf("check after the resume = %v, want nothing pending", outcome)
	}
}

// Input that arrives while a run holds the session is refused as busy, and the
// run's end answers it.
func TestDiscussRunEndAnswersInputThatFoundItBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	held := &discussStep{reached: make(chan struct{}), gate: make(chan struct{})}
	proc := newDiscussProcess(t, env, held)
	env.write(t, passiveInput{from: alice, externalID: "m1", text: "first"})
	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitReached(t, held)

	second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "arrived while busy"})
	if outcome, _ := proc.admit(t, env); outcome != pending.DiscussBusy {
		t.Fatalf("check while held = %v, want busy", outcome)
	}
	close(held.gate)
	waitDiscussRun(t, done)

	env.waitRunState(t, env.invocation(second), "completed")
	requests := proc.provider.recorded()
	if !strings.Contains(requestText(t, requests[len(requests)-1]), "arrived while busy") {
		t.Fatal("the follow-up run did not see the input that found the session busy")
	}
}

// racingAdmitter lets a test act between a discuss check's reads and its
// admission.
type racingAdmitter struct {
	*sessionruntime.Manager
	once   sync.Once
	before func()
}

func (r *racingAdmitter) Admit(ctx context.Context, in sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	r.once.Do(r.before)
	return r.Manager.Admit(ctx, in)
}

// A check whose cover position went stale before its admission ends its run
// without calling the model: a newer batch's run already answered its input.
func TestDiscussStaleBatchEndsWithoutAnswering(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	var fast *discussProcess
	var second int64
	slow := newDiscussProcessWith(t, env, []discussOption{discussAdmitter(func(manager *sessionruntime.Manager) runreq.TurnAdmitter {
		return &racingAdmitter{Manager: manager, before: func() {
			second = env.write(t, passiveInput{from: alice, externalID: "m2", text: "second"})
			outcome, done := fast.admit(t, env)
			if outcome != pending.DiscussAdmitted {
				t.Errorf("newer batch outcome = %v, want admitted", outcome)
				return
			}
			waitDiscussRun(t, done)
			env.waitRunState(t, env.invocation(second), "completed")
		}}
	})})
	fast = newDiscussProcess(t, env)
	first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "first"})

	if outcome, done := slow.admit(t, env); outcome != pending.DiscussNothingPending || done != nil {
		t.Fatalf("stale batch outcome = %v, want nothing pending", outcome)
	}
	env.waitRunState(t, env.invocation(first), "completed")
	if calls := len(slow.provider.recorded()); calls != 0 {
		t.Fatalf("the stale batch called the model %d times, want 0", calls)
	}
	if got := env.cover(t); got != second {
		t.Fatalf("cover = %d, want the newer batch's %d", got, second)
	}
}

// A new discuss run is configured with the context budget of its model and
// with the id of the run admitted for it, which compaction and the tools read
// from the configuration.
func TestDiscussRunConfigCarriesBudgetAndRunID(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	var (
		mu  sync.Mutex
		cfg native.RunConfig
	)
	proc := newDiscussProcessWith(t, env, []discussOption{discussObserveConfig(func(c native.RunConfig) {
		mu.Lock()
		defer mu.Unlock()
		cfg = c
	})}, &discussStep{})
	alice := env.participant(t, false)
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "hello"})

	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	run := env.waitRunState(t, env.invocation(hw), "completed")

	mu.Lock()
	defer mu.Unlock()
	if cfg.ContextBudgetMaxTokens != 128000 {
		t.Fatalf("ContextBudgetMaxTokens = %d, want the model's window of 128000", cfg.ContextBudgetMaxTokens)
	}
	if cfg.RunID != run.runID {
		t.Fatalf("RunID = %q, want the admitted run %q", cfg.RunID, run.runID)
	}
}

// requestIDLogs records, for each record, the request id of the context it was
// logged with.
type requestIDLogs struct {
	mu      sync.Mutex
	records []requestIDRecord
}

type requestIDRecord struct {
	message   string
	requestID string
}

func (*requestIDLogs) Enabled(context.Context, slog.Level) bool { return true }

func (l *requestIDLogs) Handle(ctx context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, requestIDRecord{message: record.Message, requestID: logger.RequestIDFromContext(ctx)})
	return nil
}

func (l *requestIDLogs) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *requestIDLogs) WithGroup(string) slog.Handler { return l }

func (l *requestIDLogs) idsOf(message string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ids []string
	for _, record := range l.records {
		if record.message == message {
			ids = append(ids, record.requestID)
		}
	}
	return ids
}

// A discuss run answers a batch of messages, not the request of the one that
// woke it, so the run and its result record carry a request id of their own:
// a different one for each run, and none of the id of whoever triggered the
// admission. Admission is reached from an input being committed, from a run
// ending and from the periodic sweep, and each of them passes through here.
func TestDiscussRunHasItsOwnRequestID(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	logs := &requestIDLogs{}
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussDeps(func(d *Deps) { d.Logger = slog.New(logs) }),
	})
	alice := env.participant(t, false)

	triggers := []string{"trigger-wake", "trigger-sweep"}
	for i, trigger := range triggers {
		hw := env.write(t, passiveInput{from: alice, externalID: fmt.Sprintf("m%d", i), text: "hello"})
		proc.answerFrom(t, trigger, env, hw)
	}

	ids := logs.idsOf("agent run")
	if len(ids) != len(triggers) {
		t.Fatalf("agent run records = %v, want one per run", ids)
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] || ids[0] == triggers[0] || ids[1] == triggers[1] {
		t.Fatalf("request ids of the runs = %v, want a different id of their own for each", ids)
	}
}

var (
	driveDiscussBotName = regexp.MustCompile(`turn-admission-[0-9a-f-]{36}`)
	driveDiscussUUID    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

// A discuss run starts from the rows of an IM group conversation, one user row
// for each message, each carrying who wrote it, what it answered and whether it
// addressed the bot. The run is admitted for the newest of them, and the model
// is shown all of them, the older ones as history, and told it speaks only
// through send. The rows are read from PostgreSQL; the request is what the
// model provider receives, with the identifiers the database drew replaced by
// fixed ones.
func TestDriveDiscussRowsFromPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration")
	}
	env := newDiscussEnv(t, "group")
	proc := newDiscussProcess(t, env, &discussStep{})
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	alice, bob, carol := env.participant(t, false), env.participant(t, false), env.participant(t, false)

	// A message from the day before: no time window drops it.
	env.write(t, passiveInput{from: alice, externalID: "m0", text: imGroupHeader(imAlice, "m0", "kickoff: freeze is Friday", at.Add(-26*time.Hour), nil)})
	env.write(t, passiveInput{from: alice, externalID: "m1", text: imGroupHeader(imAlice, "m1", "did anyone check the deploy?", at, nil)})
	env.write(t, passiveInput{from: bob, externalID: "m2", text: imGroupHeader(imBob, "m2", "I did, it passed", at.Add(time.Minute), func(in *turn.UserMessageHeaderInput) {
		in.ReplyToMessageID, in.ReplyToSender, in.ReplyToPreview = "m1", "Alice", "did anyone check the deploy?"
	})})
	hw := env.write(t, passiveInput{from: carol, externalID: "m3", mentionsMe: true, text: imGroupHeader(imCarol, "m3", "@memoh when is the freeze?", at.Add(2*time.Minute), func(in *turn.UserMessageHeaderInput) {
		in.MentionsMe = true
	})})

	outcome, done := proc.admit(t, env)
	if outcome != pending.DiscussAdmitted {
		t.Fatalf("outcome = %v, want admitted", outcome)
	}
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(hw), "completed")

	requests := proc.provider.recorded()
	if len(requests) == 0 {
		t.Fatal("the run did not reach the model provider")
	}
	got := marshalDriveRequests(t, requests[:1])
	got = driveDiscussBotName.ReplaceAll(got, []byte("<bot-name>"))
	got = bytes.ReplaceAll(got, []byte(env.botID), []byte("<bot-id>"))
	got = bytes.ReplaceAll(got, []byte(env.sessionID), []byte("<session-id>"))
	got = bytes.ReplaceAll(got, []byte(env.routeID), []byte("<route-id>"))
	got = bytes.ReplaceAll(got, []byte(env.ownerID), []byte("<owner-id>"))
	if leftover := driveDiscussUUID.Find(got); leftover != nil {
		t.Fatalf("the request carries an identifier the database drew: %s", leftover)
	}
	assertDriveGolden(t, "drive-requests", "discuss_rows", got)
}
