package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
)

type runTurn struct {
	turnID   string
	position int64
}

func (e discussEnv) runTurn(t *testing.T, invocation string) runTurn {
	t.Helper()
	var rt runTurn
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT turn_id::text, turn_position FROM session_runs WHERE session_id = $1 AND invocation_id = $2`,
		e.sessionID, invocation).Scan(&rt.turnID, &rt.position))
	return rt
}

func (e discussEnv) counter(t *testing.T) int64 {
	t.Helper()
	var next int64
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT next_turn_position FROM bot_sessions WHERE id = $1`, e.sessionID).Scan(&next))
	return next
}

func (e discussEnv) rowsOfRun(t *testing.T, invocation string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT m.turn_id::text FROM bot_history_messages m
		JOIN session_runs r ON r.run_id = m.run_id
		WHERE r.session_id = $1 AND r.invocation_id = $2`, e.sessionID, invocation)
	require.NoError(t, err)
	defer rows.Close()
	var turns []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		turns = append(turns, id)
	}
	require.NoError(t, rows.Err())
	return turns
}

// A discuss run answers the batch head's request row, so it joins that row's
// turn: no turn nobody writes to, and no position taken from the counter.
func TestDiscussRunJoinsTheBatchHeadTurn(t *testing.T) {
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{send: "on it"})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "long task"})
	var headTurn string
	require.NoError(t, env.pool.QueryRow(context.Background(),
		`SELECT turn_id::text FROM bot_history_messages WHERE session_id = $1 AND turn_position = $2 AND role = 'user'`,
		env.sessionID, hw).Scan(&headTurn))
	before := env.counter(t)

	outcome, done := proc.admit(t, env)
	require.Equal(t, pending.DiscussAdmitted, outcome)
	waitDiscussRun(t, done)
	env.waitRunState(t, env.invocation(hw), "completed")

	got := env.runTurn(t, env.invocation(hw))
	require.Equal(t, runTurn{headTurn, hw}, got, "the run takes the head row's turn")
	require.Equal(t, before, env.counter(t), "joining a turn draws no position")
	rows := env.rowsOfRun(t, env.invocation(hw))
	require.NotEmpty(t, rows)
	for _, turnID := range rows {
		require.Equal(t, headTurn, turnID, "every row the run wrote is in its turn")
	}
}

// interruptedDiscussRun answers one passive message and marks that run lost, the
// way a restart leaves it, and returns it with the turn it ran in.
func interruptedDiscussRun(t *testing.T, env discussEnv) (runID, invocation string, original runTurn) {
	t.Helper()
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{send: "starting"})
	hw := env.write(t, passiveInput{from: alice, externalID: "m1", text: "long task"})
	outcome, done := proc.admit(t, env)
	require.Equal(t, pending.DiscussAdmitted, outcome)
	waitDiscussRun(t, done)
	interrupted := env.waitRunState(t, env.invocation(hw), "completed")
	_, err := env.pool.Exec(context.Background(), `UPDATE session_runs SET state = 'lost', error_code = 'session_runtime.interrupted' WHERE run_id = $1`, interrupted.runID)
	require.NoError(t, err)
	return interrupted.runID, env.invocation(hw), env.runTurn(t, env.invocation(hw))
}

// resumeInterrupted starts the continuation the ledger owes for runID in a
// process of its own and waits for it to end.
func resumeInterrupted(t *testing.T, env discussEnv, runID string, steps ...*discussStep) {
	t.Helper()
	ctx := context.Background()
	rows, err := dbsqlc.New(env.pool).ListInterruptedSessionRuns(ctx, db.ParseUUIDOrEmpty(uuid.Nil.String()))
	require.NoError(t, err)
	var row dbsqlc.SessionRun
	for _, candidate := range rows {
		if candidate.RunID.String() == runID {
			row = candidate
		}
	}
	require.True(t, row.RunID.Valid, "the interrupted run is owed a continuation")
	proc := newDiscussProcessWith(t, env, []discussOption{
		discussTeam(row.TeamID.String()),
		discussDeps(func(d *Deps) {
			d.Accounts = accounts.NewService(nil, titleModelAccountStore{account: dbstore.AccountRecord{ID: env.ownerID, IsActive: true}})
			d.BotPermissions = &agenttest.PermissionChecker{Values: map[string]bool{env.botID + ":" + env.ownerID + ":" + bots.PermissionChat: true}}
		}),
	}, steps...)
	resumed, err := startContinuation(ctx, proc.svc, row)
	require.NoError(t, err)
	waitDiscussRun(t, resumed)
}

// A continuation joins the interrupted run's turn and takes over its intent:
// interrupted again, only the continuation is owed.
func TestContinuationJoinsTheInterruptedTurnAndOnlyTheLatestIsOwed(t *testing.T) {
	ctx := context.Background()
	env := newDiscussEnv(t, "group")
	runID, _, original := interruptedDiscussRun(t, env)
	before := env.counter(t)

	resumeInterrupted(t, env, runID)

	invocation := "resume:" + runID
	require.Equal(t, original, env.runTurn(t, invocation), "the continuation takes the interrupted run's turn")
	require.Equal(t, before, env.counter(t), "joining a turn draws no position")
	for _, turnID := range env.rowsOfRun(t, invocation) {
		require.Equal(t, original.turnID, turnID)
	}

	// Interrupted a second time, it alone is owed: the source's intent ended
	// with the admission, though both runs hold the same position.
	_, err := env.pool.Exec(ctx, `
		UPDATE session_runs SET state = 'lost', error_code = 'session_runtime.interrupted',
		       input_json = input_json || '{"resume":{"version":1,"chat_id":"c"}}'::jsonb
		WHERE session_id = $1 AND invocation_id = $2`, env.sessionID, invocation)
	require.NoError(t, err)
	var owed []string
	rows, err := dbsqlc.New(env.pool).ListInterruptedSessionRuns(ctx, db.ParseUUIDOrEmpty(uuid.Nil.String()))
	require.NoError(t, err)
	for _, candidate := range rows {
		if candidate.SessionID.String() == env.sessionID {
			owed = append(owed, candidate.InvocationID)
		}
	}
	require.Equal(t, []string{invocation}, owed)
}

// A steer applied inside the interrupted run opens a later turn, and the
// continuation answers that turn's request row. It joins the turn that row
// belongs to, so every row it writes is in its own turn and no position is
// drawn.
func TestContinuationJoinsTheTurnOfTheRequestItAnswers(t *testing.T) {
	ctx := context.Background()
	env := newDiscussEnv(t, "group")
	runID, _, original := interruptedDiscussRun(t, env)

	var steerTurn runTurn
	steerTurn.turnID = uuid.NewString()
	require.NoError(t, env.pool.QueryRow(ctx, `
		UPDATE bot_sessions SET next_turn_position = next_turn_position + 1
		WHERE id = $1 RETURNING next_turn_position - 1`, env.sessionID).Scan(&steerTurn.position))
	_, err := env.pool.Exec(ctx, `
		INSERT INTO bot_history_messages (id, bot_id, session_id, role, content, run_id, turn_id, turn_position, turn_message_seq, turn_visible)
		VALUES ($1, $2, $3, 'user', '{"role":"user","content":"steer"}', $4, $5, $6, 1, true)`,
		uuid.NewString(), env.botID, env.sessionID, runID, steerTurn.turnID, steerTurn.position)
	require.NoError(t, err)
	require.Greater(t, steerTurn.position, original.position)
	before := env.counter(t)

	resumeInterrupted(t, env, runID, &discussStep{send: "resumed"})

	invocation := "resume:" + runID
	require.Equal(t, steerTurn, env.runTurn(t, invocation), "the continuation joins the steer's turn, not the run's root turn")
	require.Equal(t, before, env.counter(t), "joining a turn draws no position")
	written := env.rowsOfRun(t, invocation)
	require.NotEmpty(t, written)
	for _, turnID := range written {
		require.Equal(t, steerTurn.turnID, turnID, "every row the continuation wrote is in its own turn")
	}
}
