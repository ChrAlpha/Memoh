package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/resume"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/bots"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/team"
)

// These tests are the cold start of a server: the process that ran a session was
// stopped, the session is owed the continuation of its interrupted run and
// perhaps queued input, and the new process comes up before its workspace does.
// The ledger, the queue and the invocation index are PostgreSQL's.

// workspaceGate is the readiness check of a process: it answers as ready says,
// records what it was asked, and can hold a check until released.
type workspaceGate struct {
	ready atomic.Bool

	mu    sync.Mutex
	asked [][2]string
	block chan struct{}
}

func (g *workspaceGate) check(ctx context.Context, botID, targetID string) error {
	g.mu.Lock()
	g.asked = append(g.asked, [2]string{botID, targetID})
	block := g.block
	g.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if g.ready.Load() {
		return nil
	}
	return errors.New("workspace not ready")
}

func (g *workspaceGate) checks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.asked)
}

func (g *workspaceGate) lastAsked() [2]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked[len(g.asked)-1]
}

// untilChecks waits for the gate to have been asked n times, which is how a test
// knows an asynchronous trigger has reached the start of work. A sweep may skip
// a session whose batch another trigger holds claimed, so a test waits for one
// ask, not one per trigger.
func (g *workspaceGate) untilChecks(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return g.checks() >= n }, 20*time.Second, 5*time.Millisecond)
}

// interruptedRun admits a run of the session on proc, saves its resume intent
// as a streaming turn does, and stops it with the process's graceful shutdown.
func interruptedRun(t *testing.T, proc *discussProcess, pool *pgxpool.Pool, botID, sessionID, userID, task, targetID string) string {
	t.Helper()
	ctx := context.Background()
	admission, err := proc.manager.Admit(ctx, sessionruntime.AdmitInput{
		BotID: botID, SessionID: sessionID, InvocationID: uuid.NewString(),
		Payload: []byte(`{"kind":"message"}`),
		Execution: sessionruntime.Execution{Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		}},
	})
	require.NoError(t, err)
	require.True(t, admission.Started)
	// The request row a chat turn writes with its admission, in the run's turn.
	_, err = pool.Exec(ctx, `INSERT INTO bot_history_messages (id, bot_id, session_id, role, content, run_id, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, $3, 'user', '{}', $4, $5, $6, 1, true)`, uuid.NewString(), botID, sessionID, admission.RunID, admission.TurnID, admission.TurnPosition)
	require.NoError(t, err)
	fenced := runtimefence.WithContext(ctx, runtimefence.Fence{BotID: botID, SessionID: sessionID, Token: admission.Handle.FencingToken})
	require.NoError(t, proc.svc.admit.RecordResume(fenced, runreq.ChatRequest{RunID: admission.RunID, ChatID: botID, UserID: userID, Query: task, WorkspaceTargetID: targetID}))
	require.NoError(t, proc.manager.InterruptForShutdown(ctx))
	// The stopped process's own terminal callback asks what waits on the session,
	// off its goroutine. A shutdown waits for that ask as it waits for the
	// active turns; it is not the restarted process under test.
	require.NoError(t, proc.svc.DrainActiveTurns(ctx))
	return admission.RunID
}

// sessionRun is one run of the session as the ledger holds it.
type sessionRun struct {
	invocation, state string
	position          int64
	created           time.Time
	hasResumeIntent   bool
}

func sessionRuns(t *testing.T, env discussEnv) []sessionRun {
	t.Helper()
	rows, err := env.pool.Query(context.Background(), `
		SELECT invocation_id, state || COALESCE(':' || error_code || ':' || error_message, ''), turn_position, created_at, input_json ? 'resume'
		FROM session_runs WHERE session_id = $1 ORDER BY turn_position, created_at
	`, env.sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var runs []sessionRun
	for rows.Next() {
		var run sessionRun
		require.NoError(t, rows.Scan(&run.invocation, &run.state, &run.position, &run.created, &run.hasResumeIntent))
		runs = append(runs, run)
	}
	require.NoError(t, rows.Err())
	return runs
}

func untilRuns(t *testing.T, env discussEnv, want func([]sessionRun) bool) []sessionRun {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		runs := sessionRuns(t, env)
		if want(runs) {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session's runs did not reach the expected state: %+v", runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func allCompleted(n int) func([]sessionRun) bool {
	return func(runs []sessionRun) bool {
		if len(runs) != n {
			return false
		}
		for _, run := range runs {
			if run.state != "completed" && run.state != "lost" {
				return false
			}
		}
		return true
	}
}

// restartedQueueProcess is the process the server restarts into: it serves the
// team, and gate decides whether the workspace is up.
func restartedQueueProcess(t *testing.T, env queueEnv, gate *workspaceGate, steps ...*discussStep) *discussProcess {
	t.Helper()
	return newQueueProcessWith(t, env, []discussOption{discussService(withWorkspaceReadiness(gate.check)), discussDeps(func(d *Deps) {
		d.BotPermissions = &agenttest.PermissionChecker{Values: map[string]bool{
			env.botID + ":" + env.ownerID + ":" + bots.PermissionChat:          true,
			env.botID + ":" + env.ownerID + ":" + bots.PermissionWorkspaceRead: true,
		}}
	})}, steps...)
}

// triggers fires everything that asks what waits on the session: the queue
// sweep, the resume scan and a kick, together.
func triggerRestartAdmission(t *testing.T, proc *discussProcess, env queueEnv) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, trigger := range []func(){
		func() { proc.svc.pending.SweepQueueScope(ctx, team.DefaultTeamID, time.Now()) },
		func() {
			scan := resume.Scan{}
			if err := proc.svc.resume.ScanInterrupted(ctx, &scan); err != nil {
				t.Error(err)
			}
		},
		func() { proc.svc.pending.Kick(ctx, env.botID, env.sessionID) },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); trigger() }()
	}
	wg.Wait()
}

// A session whose run was interrupted and that has a follow-up queued behind
// it starts nothing while its workspace is down: not the continuation, and not
// the follow-up that the continuation is owed ahead of. Once the workspace is
// up the continuation starts first, and the follow-up when it has ended.
func TestColdStartHoldsTheQueueBehindTheContinuationUntilTheWorkspaceIsReady(t *testing.T) {
	env := newQueueEnv(t)
	interrupted := interruptedRun(t, newQueueProcess(t, env), env.pool, env.botID, env.sessionID, env.ownerID, "the long task", "ws-1")
	env.park(t, "f1", "", "a follow-up typed before the restart")

	gate := &workspaceGate{}
	proc := restartedQueueProcess(t, env, gate, &discussStep{text: "continued"}, &discussStep{text: "answered the follow-up"})

	triggerRestartAdmission(t, proc, env)
	gate.untilChecks(t, 1)
	outcome, err := proc.svc.pending.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	require.Equal(t, pending.DiscussSkipped, outcome, "a session whose workspace is down waits")

	runs := sessionRuns(t, env.discussEnv)
	require.Len(t, runs, 1, "nothing was admitted besides the interrupted run")
	require.True(t, runs[0].hasResumeIntent, "the interrupted run is still owed its continuation")
	// A trigger still in flight may hold the batch claimed for a moment; it ends
	// released, never consumed or failed.
	require.Eventually(t, func() bool { return env.stateOf(t, "f1") == "queued" }, 10*time.Second, 10*time.Millisecond)
	require.Equal(t, [2]string{env.botID, "ws-1"}, gate.lastAsked(), "the workspace asked about is the intent's target")
	require.Empty(t, modelTexts(t, proc))

	gate.ready.Store(true)
	outcome, err = proc.svc.pending.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	require.Equal(t, pending.DiscussBusy, outcome)

	runs = untilRuns(t, env.discussEnv, allCompleted(3))
	require.Equal(t, "resume:"+interrupted, runs[1].invocation, "the continuation is the first run after the interrupted one")
	require.Equal(t, runs[0].position, runs[1].position, "the continuation joins the interrupted run's turn")
	require.Contains(t, runs[2].invocation, "queued:", "the follow-up starts after it")
	require.True(t, runs[2].created.After(runs[1].created))
	require.Greater(t, runs[2].position, runs[1].position)
	env.waitState(t, "f1", "consumed")
}

// The same for a discuss session: its interrupted run is continued with its
// original cover position before the external input that arrived since is
// answered.
func TestColdStartHoldsDiscussInputBehindTheContinuationUntilTheWorkspaceIsReady(t *testing.T) {
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	first := env.write(t, passiveInput{from: alice, externalID: "m1", text: "long task"})
	before := newDiscussProcess(t, env, &discussStep{send: "starting"})
	outcome, done := before.admit(t, env)
	require.Equal(t, pending.DiscussAdmitted, outcome)
	waitDiscussRun(t, done)
	interrupted := env.waitRunState(t, env.invocation(first), "completed")
	// The run was cut off by a shutdown instead.
	_, err := env.pool.Exec(context.Background(), `
		UPDATE session_runs SET state = 'lost', error_code = 'session_runtime.interrupted' WHERE run_id = $1
	`, interrupted.runID)
	require.NoError(t, err)
	second := env.write(t, passiveInput{from: alice, externalID: "m2", text: "asked again while the server was down"})

	gate := &workspaceGate{}
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussTeam(team.DefaultTeamID),
		discussDeps(func(d *Deps) {
			d.Accounts = accounts.NewService(nil, titleModelAccountStore{account: dbstore.AccountRecord{ID: env.ownerID, IsActive: true}})
			d.BotPermissions = &agenttest.PermissionChecker{Values: map[string]bool{env.botID + ":" + env.ownerID + ":" + bots.PermissionChat: true}}
		}),
		discussService(withWorkspaceReadiness(gate.check)),
	}, &discussStep{send: "continued"}, &discussStep{send: "answered the new input"})

	ctx := context.Background()
	var wg sync.WaitGroup
	for _, trigger := range []func(){
		func() { pending.NewDiscussSweep(proc.svc.pending).Sweep(ctx, time.Now()) },
		func() {
			scan := resume.Scan{}
			if err := proc.svc.resume.ScanInterrupted(ctx, &scan); err != nil {
				t.Error(err)
			}
		},
		func() { proc.svc.pending.Kick(ctx, env.botID, env.sessionID) },
	} {
		wg.Add(1)
		go func() { defer wg.Done(); trigger() }()
	}
	wg.Wait()
	gate.untilChecks(t, 1)
	_, err = proc.svc.pending.CheckAndAdmitDiscuss(ctx, env.botID, env.sessionID)
	require.NoError(t, err)

	runs := sessionRuns(t, env)
	require.Len(t, runs, 1, "nothing was admitted besides the interrupted run")
	require.True(t, runs[0].hasResumeIntent)
	require.Zero(t, env.cover(t))

	gate.ready.Store(true)
	outcome, err = proc.svc.pending.CheckAndAdmitDiscuss(ctx, env.botID, env.sessionID)
	require.NoError(t, err)
	require.Equal(t, pending.DiscussBusy, outcome)

	runs = untilRuns(t, env, allCompleted(3))
	require.Equal(t, "resume:"+interrupted.runID, runs[1].invocation)
	var resumedCover int64
	require.NoError(t, env.pool.QueryRow(ctx, `
		SELECT COALESCE((input_json->>'cover_position')::bigint, 0) FROM session_runs WHERE invocation_id = $1
	`, "resume:"+interrupted.runID).Scan(&resumedCover))
	require.Equal(t, first, resumedCover, "the continuation keeps the cover position of the run it continues")
	require.Equal(t, env.invocation(second), runs[2].invocation, "the new input is answered after the continuation")
	require.Greater(t, runs[2].position, runs[1].position)
	require.Equal(t, second, env.cover(t))
}

// A user who writes to a session that is owed a continuation is served at once,
// whether or not the workspace is ready, and has taken the session over: the
// continuation is retired and never runs, and the follow-up queued behind the
// user's turn starts when that turn ends.
func TestUserMessageOnASessionOwedAContinuationTakesTheSessionOver(t *testing.T) {
	env := newQueueEnv(t)
	interrupted := interruptedRun(t, newQueueProcess(t, env), env.pool, env.botID, env.sessionID, env.ownerID, "the long task", "")

	gate := &workspaceGate{}
	held := heldStep("the user's answer")
	proc := restartedQueueProcess(t, env, gate, held, &discussStep{text: "answered the follow-up"})

	release := startHeldRun(t, proc, env, held)
	require.Zero(t, gate.checks(), "a turn the user starts does not wait for the workspace check")
	_, err := proc.svc.EnqueueFollowUp(context.Background(), env.input("f1", "typed while the answer is running"))
	require.NoError(t, err)

	scan := resume.Scan{}
	require.NoError(t, proc.svc.resume.ScanInterrupted(context.Background(), &scan))
	require.False(t, sessionRuns(t, env.discussEnv)[0].hasResumeIntent, "the intent is retired")

	gate.ready.Store(true)
	release()
	runs := untilRuns(t, env.discussEnv, allCompleted(3))
	for _, run := range runs {
		require.NotEqual(t, "resume:"+interrupted, run.invocation, "the retired continuation never runs")
	}
	require.Contains(t, runs[2].invocation, "queued:")
	env.waitState(t, "f1", "consumed")
}

// A continuation that can never run is retired, and the follow-up behind it
// starts in the same admission, without waiting for another sweep.
func TestUnrecoverableContinuationIsRetiredAndTheFollowUpStartsInTheSameAdmission(t *testing.T) {
	for name, damage := range map[string]string{
		"execution budget spent":    `jsonb_set(input_json, '{resume,deadline_at}', to_jsonb('2020-01-01T00:00:00Z'::text))`,
		"chat route of another bot": `jsonb_set(input_json, '{resume,chat_claims}', '{"typ":"chat_route","bot_id":"another-bot","chat_id":"c"}'::jsonb)`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newQueueEnv(t)
			interrupted := interruptedRun(t, newQueueProcess(t, env), env.pool, env.botID, env.sessionID, env.ownerID, "the long task", "")
			_, err := env.pool.Exec(context.Background(), `UPDATE session_runs SET input_json = `+damage+` WHERE run_id = $1`, interrupted)
			require.NoError(t, err)
			env.park(t, "f1", "", "a follow-up typed before the restart")

			gate := &workspaceGate{}
			gate.ready.Store(true)
			proc := restartedQueueProcess(t, env, gate, &discussStep{text: "answered the follow-up"})

			outcome, err := proc.svc.pending.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID)
			require.NoError(t, err)
			require.Equal(t, pending.DiscussBusy, outcome, "the follow-up was started by the same admission")

			runs := untilRuns(t, env.discussEnv, allCompleted(2))
			require.False(t, runs[0].hasResumeIntent, "the intent is retired")
			require.Contains(t, runs[1].invocation, "queued:")
			env.waitState(t, "f1", "consumed")
		})
	}
}

// However many triggers ask at once, the session starts one run, and it is the
// continuation: the follow-up does not start until the continuation ends.
func TestConcurrentTriggersAdmitOnlyTheContinuation(t *testing.T) {
	env := newQueueEnv(t)
	interrupted := interruptedRun(t, newQueueProcess(t, env), env.pool, env.botID, env.sessionID, env.ownerID, "the long task", "")
	env.park(t, "f1", "", "a follow-up typed before the restart")

	gate := &workspaceGate{}
	gate.ready.Store(true)
	held := heldStep("continued")
	proc := restartedQueueProcess(t, env, gate, held, &discussStep{text: "answered the follow-up"})

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			triggerRestartAdmission(t, proc, env)
			if _, err := proc.svc.pending.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	waitReached(t, held)

	runs := sessionRuns(t, env.discussEnv)
	require.Len(t, runs, 2, "the interrupted run and one continuation")
	require.Equal(t, "resume:"+interrupted, runs[1].invocation)
	require.Equal(t, "queued", env.stateOf(t, "f1"))

	close(held.gate)
	untilRuns(t, env.discussEnv, allCompleted(3))
}

// Queued input with nothing interrupted is also held while the workspace is
// down, without a failure row, and starts once it is up.
func TestQueuedFollowUpWaitsForTheWorkspaceWithoutFailing(t *testing.T) {
	env := newQueueEnv(t)
	env.park(t, "f1", "", "typed before the restart")
	gate := &workspaceGate{}
	proc := restartedQueueProcess(t, env, gate, &discussStep{text: "answered"})

	triggerRestartAdmission(t, proc, env)
	gate.untilChecks(t, 1)
	outcome, err := proc.svc.pending.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	require.Equal(t, pending.DiscussSkipped, outcome)

	require.Empty(t, sessionRuns(t, env.discussEnv), "no run was admitted")
	require.Eventually(t, func() bool { return env.stateOf(t, "f1") == "queued" }, 10*time.Second, 10*time.Millisecond)
	var rows int
	require.NoError(t, env.pool.QueryRow(context.Background(), `SELECT count(*) FROM bot_history_messages WHERE session_id = $1`, env.sessionID).Scan(&rows))
	require.Zero(t, rows, "no failed turn was written for the input")

	gate.ready.Store(true)
	_, err = proc.svc.pending.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	env.waitQueuedRuns(t, 1)
	env.waitState(t, "f1", "consumed")
}

// A workspace check that does not return holds the session's admission, and
// nothing else: queueing input, a run's terminal callback and a turn the user
// starts all return at once.
func TestBlockedWorkspaceCheckDoesNotBlockQueueingOrTerminalCallbacks(t *testing.T) {
	env := newQueueEnv(t)
	interruptedRun(t, newQueueProcess(t, env), env.pool, env.botID, env.sessionID, env.ownerID, "the long task", "")
	env.park(t, "f1", "", "typed before the restart")

	gate := &workspaceGate{block: make(chan struct{})}
	proc := restartedQueueProcess(t, env, gate, &discussStep{text: "answered"})
	t.Cleanup(func() { close(gate.block) })

	proc.svc.pending.Kick(context.Background(), env.botID, env.sessionID)
	gate.untilChecks(t, 1)

	returned := make(chan error, 3)
	go func() {
		_, err := proc.svc.pending.Enqueue(context.Background(), sessionruntime.QueueEnqueue{
			Key: env.key(), Origin: sessionruntime.QueueOriginFollowUp, InvocationID: "f2",
			Payload: queuedPayload(t, env.turnCommand("f2", "typed while the check hangs"), time.Now()),
		})
		returned <- err
	}()
	go func() {
		proc.svc.pending.AfterSessionTerminal(context.Background(), sessionruntime.TerminalRun{BotID: env.botID, SessionID: env.sessionID})
		returned <- nil
	}()
	go func() {
		_, err := proc.svc.StartTurn(context.Background(), env.turnCommand("late", "arrives while the check hangs"))
		returned <- err
	}()
	for range 3 {
		select {
		case err := <-returned:
			if err != nil {
				require.ErrorIs(t, err, turn.ErrSessionBusy)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a call waited for the workspace check")
		}
	}
	require.Equal(t, "queued", env.stateOf(t, "f1"))
}

// A process runs at most four continuations at once, however many sessions are
// owed one: after a rolling deploy that is the back-pressure on the model and
// the workspaces. The session that finds no slot keeps its intent, and its
// queued follow-up does not start ahead of it; a slot freed by an ended
// continuation lets the next trigger start it.
func TestContinuationsRunFourAtATimeAndTheFifthSessionWaitsForASlot(t *testing.T) {
	const sessions = 5
	envs := make([]queueEnv, sessions)
	owners := map[string]bool{}
	botOf := map[string]string{}
	for i := range envs {
		envs[i] = newQueueEnv(t)
		botOf[envs[i].sessionID] = envs[i].botID
		owners[envs[i].botID+":"+envs[i].ownerID+":"+bots.PermissionChat] = true
		interruptedRun(t, newQueueProcess(t, envs[i]), envs[i].pool, envs[i].botID, envs[i].sessionID, envs[i].ownerID, "the long task", "")
		envs[i].park(t, "f1", "", "a follow-up typed before the restart")
	}
	steps := make([]*discussStep, 0, sessions+2)
	for range 4 {
		steps = append(steps, heldStep("continued"))
	}
	steps = append(steps, &discussStep{text: "continued"}, &discussStep{text: "answered"})

	gate := &workspaceGate{}
	gate.ready.Store(true)
	proc := newQueueProcessWith(t, envs[0], []discussOption{
		discussService(withWorkspaceReadiness(gate.check)),
		discussDeps(func(d *Deps) {
			d.BotPermissions = &agenttest.PermissionChecker{Values: owners}
			d.Sessions = &agenttest.Sessions{
				GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{
						ID: id, BotID: botOf[id], Type: sessionpkg.TypeChat, SessionMode: sessionpkg.TypeChat,
						RuntimeType: sessionpkg.RuntimeModel, Facts: sessionfacts.Preset(sessionpkg.TypeChat),
					}, nil
				},
				UpdateMetadataFn: func(_ context.Context, id string, _ map[string]any) (sessionpkg.Thread, error) {
					return sessionpkg.Thread{ID: id}, nil
				},
			}
		}),
	}, steps...)

	ctx := context.Background()
	scan := resume.Scan{}
	require.NoError(t, proc.svc.resume.ScanInterrupted(ctx, &scan))
	for _, step := range steps[:4] {
		waitReached(t, step)
	}
	resumed := func() (running, waiting []queueEnv) {
		for _, env := range envs {
			started := false
			for _, run := range sessionRuns(t, env.discussEnv) {
				started = started || strings.HasPrefix(run.invocation, "resume:")
			}
			if started {
				running = append(running, env)
			} else {
				waiting = append(waiting, env)
			}
		}
		return running, waiting
	}
	// Give any further continuation the time to be wrongly admitted.
	require.Never(t, func() bool { running, _ := resumed(); return len(running) > 4 }, 500*time.Millisecond, 20*time.Millisecond)
	running, waiting := resumed()
	require.Len(t, running, 4)
	require.Len(t, waiting, 1)
	fifth := waiting[0]
	require.True(t, sessionRuns(t, fifth.discussEnv)[0].hasResumeIntent, "the fifth session is still owed its continuation")
	require.Equal(t, "queued", fifth.stateOf(t, "f1"), "and its follow-up did not start ahead of it")

	close(steps[0].gate)
	require.Eventually(t, func() bool {
		proc.svc.pending.Kick(context.Background(), fifth.botID, fifth.sessionID)
		for _, run := range sessionRuns(t, fifth.discussEnv) {
			if strings.HasPrefix(run.invocation, "resume:") {
				return true
			}
		}
		return false
	}, 20*time.Second, 50*time.Millisecond, "a freed slot lets the fifth continuation start")
	for _, step := range steps[1:4] {
		close(step.gate)
	}
}
