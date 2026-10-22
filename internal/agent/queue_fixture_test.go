package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/team"
)

// These tests run the session input queue and the runs it starts against
// PostgreSQL: the queue's claim, the ledger's single slot and the invocation
// index are PostgreSQL's, the model is scripted. Each case is skipped unless
// TEST_POSTGRES_DSN names a migrated database.

// queueEnv is one chat session reached through a Telegram route.
type queueEnv struct {
	discussEnv
	store *sessionruntime.QueuedInputs
}

func newQueueEnv(t *testing.T) queueEnv {
	t.Helper()
	env := newDiscussEnv(t, "private")
	_, err := env.pool.Exec(context.Background(), `
		UPDATE bot_sessions
		SET type = 'chat', session_mode = 'chat', reply_delivery = 'stream', trigger_scope = 'addressed_message'
		WHERE id = $1
	`, env.sessionID)
	require.NoError(t, err)
	return queueEnv{discussEnv: env, store: sessionruntime.NewQueuedInputs(dbsqlc.New(env.pool), env.pool)}
}

func (e queueEnv) key() sessionruntime.Key {
	return sessionruntime.Key{BotID: e.botID, SessionID: e.sessionID}
}

// newQueueProcess is one server process over the shared database whose model
// answers with the scripted steps.
func newQueueProcess(t *testing.T, env queueEnv, steps ...*discussStep) *discussProcess {
	t.Helper()
	return newQueueProcessWith(t, env, nil, steps...)
}

// newQueueProcessWith is newQueueProcess with options that change the process
// after its defaults: the environment's queue and the default team.
func newQueueProcessWith(t *testing.T, env queueEnv, options []discussOption, steps ...*discussStep) *discussProcess {
	t.Helper()
	base := []discussOption{
		discussDeps(func(d *Deps) { d.Sessions = chatSessionService(env.botID) }),
		discussService(withQueue(env.store), withAllowedTeam(team.DefaultTeamID)),
	}
	return newDiscussProcessWith(t, env.discussEnv, append(base, options...), steps...)
}

func chatSessionService(botID string) *agenttest.Sessions {
	return &agenttest.Sessions{
		GetFn: func(_ context.Context, id string) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{
				ID: id, BotID: botID, Type: sessionpkg.TypeChat, SessionMode: sessionpkg.TypeChat,
				RuntimeType: sessionpkg.RuntimeModel, Facts: sessionfacts.Preset(sessionpkg.TypeChat),
			}, nil
		},
		UpdateMetadataFn: func(_ context.Context, id string, _ map[string]any) (sessionpkg.Thread, error) {
			return sessionpkg.Thread{ID: id}, nil
		},
	}
}

// turnCommand is a user's message to the session as an ingress builds it.
func (e queueEnv) turnCommand(invocation, text string) turn.StartTurnCommand {
	return turn.StartTurnCommand{
		SchemaVersion: 1, TeamID: team.DefaultTeamID, Mode: turn.ModeChat,
		BotID: e.botID, ChatID: e.botID, ThreadID: e.sessionID,
		IdempotencyKey: invocation, ExternalMessageID: invocation,
		Query: text, UserVisibleText: text,
		CurrentChannel: discussTestPlatform, ReplyTarget: discussTestTarget, ConversationType: "private",
	}
}

// startHeldRun starts a run whose model call waits for the returned release.
func startHeldRun(t *testing.T, proc *discussProcess, env queueEnv, step *discussStep) (release func()) {
	t.Helper()
	cmd := env.turnCommand("first-"+env.sessionID, "the first question")
	handle, err := proc.svc.StartTurn(context.Background(), cmd)
	require.NoError(t, err)
	go drainInBackground(handle)
	waitReached(t, step)
	var once sync.Once
	return func() { once.Do(func() { close(step.gate) }) }
}

func heldStep(text string) *discussStep {
	return &discussStep{reached: make(chan struct{}), gate: make(chan struct{}), text: text}
}

// queuedRun is a run the queue started, as the ledger holds it.
type queuedRun struct{ invocation, state string }

func (e queueEnv) queuedRuns(t *testing.T) []queuedRun {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT invocation_id, state FROM session_runs
		WHERE session_id = $1 AND invocation_id LIKE 'queued:%' ORDER BY created_at, run_id
	`, e.sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var runs []queuedRun
	for rows.Next() {
		var run queuedRun
		require.NoError(t, rows.Scan(&run.invocation, &run.state))
		runs = append(runs, run)
	}
	require.NoError(t, rows.Err())
	return runs
}

// waitQueuedRuns polls until n queued runs exist and all of them completed.
func (e queueEnv) waitQueuedRuns(t *testing.T, n int) []queuedRun {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		runs := e.queuedRuns(t)
		done := len(runs) == n
		for _, run := range runs {
			done = done && run.state == "completed"
		}
		if done {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued runs = %v, want %d completed", runs, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e queueEnv) stateOf(t *testing.T, invocation string) string {
	t.Helper()
	var state string
	err := e.pool.QueryRow(context.Background(), `SELECT state FROM session_queued_inputs WHERE session_id = $1 AND invocation_id = $2`, e.sessionID, invocation).Scan(&state)
	require.NoError(t, err)
	return state
}

func (e queueEnv) waitState(t *testing.T, invocation, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if got := e.stateOf(t, invocation); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("input %s is %s, want %s", invocation, e.stateOf(t, invocation), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e queueEnv) input(invocation, text string) agentapi.QueueInput {
	return agentapi.QueueInput{
		TeamID: team.DefaultTeamID, BotID: e.botID, SessionID: e.sessionID, InvocationID: invocation,
		UserID: "", SourceChannelIdentityID: "", Text: text,
	}
}

func modelTexts(t *testing.T, proc *discussProcess) []string {
	t.Helper()
	var out []string
	for _, req := range proc.provider.recorded() {
		out = append(out, requestText(t, req))
	}
	return out
}

// queuedPayload is a stored input as a channel parks it: the turn, who it
// answers, and when it arrived.
func queuedPayload(t *testing.T, cmd turn.StartTurnCommand, at time.Time) []byte {
	t.Helper()
	payload, err := turnwire.NewQueuedInput(cmd, &auth.RouteCredentials{
		BotID: cmd.BotID, ChatID: cmd.ChatID, ReplyTarget: cmd.ReplyTarget, UserID: "user-1",
	}, at).Marshal()
	require.NoError(t, err)
	return payload
}

func (e queueEnv) park(t *testing.T, invocation, mergeKey, text string) sessionruntime.QueuedInput {
	t.Helper()
	item, err := e.store.Enqueue(context.Background(), sessionruntime.QueueEnqueue{
		Key: e.key(), Origin: sessionruntime.QueueOriginFollowUp, InvocationID: invocation, MergeKey: mergeKey,
		Payload: queuedPayload(t, e.turnCommand(invocation, text), time.Now()),
	})
	require.NoError(t, err)
	return item
}

// userRow is a user row of the session as history holds it.
type userRow struct {
	id, external, sender, display, content string
	position, seq                          int64
	reply                                  string
}

func (e queueEnv) userRows(t *testing.T) []userRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT id::text, COALESCE(source_message_id, ''), COALESCE(sender_channel_identity_id::text, ''),
		       COALESCE(display_text, ''), content::text, turn_position, turn_message_seq,
		       COALESCE(source_reply_to_message_id, '')
		FROM bot_history_messages
		WHERE session_id = $1 AND role = 'user' AND turn_visible
		ORDER BY turn_position, turn_message_seq, created_at, id
	`, e.sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var row userRow
		require.NoError(t, rows.Scan(&row.id, &row.external, &row.sender, &row.display, &row.content, &row.position, &row.seq, &row.reply))
		var stored struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(row.content), &stored); err == nil && stored.Content != "" {
			row.content = stored.Content
		}
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// parkMessage parks a message as an ingress queues it: the complete command of
// its sender, with the mention and the reply it carried.
func (e queueEnv) parkMessage(t *testing.T, id, mergeKey, text string, edit func(*turn.StartTurnCommand), at time.Time) sessionruntime.QueuedInput {
	t.Helper()
	cmd := e.turnCommand(id, text)
	cmd.SourceChannelIdentityID = mergeKey
	if edit != nil {
		edit(&cmd)
	}
	item, err := e.store.Enqueue(context.Background(), sessionruntime.QueueEnqueue{
		Key: e.key(), Origin: sessionruntime.QueueOriginFollowUp, InvocationID: id, MergeKey: mergeKey,
		Payload: queuedPayload(t, cmd, at),
	})
	require.NoError(t, err)
	return item
}

// parked puts an input in the queue without waking the drain, so a case that
// stages the live side by hand is not raced by a start of its own.
func (e queueEnv) parked(t *testing.T, invocation string) sessionruntime.FollowUpPendingRef {
	t.Helper()
	item := e.park(t, invocation, "", "promote "+invocation)
	return sessionruntime.FollowUpPendingRef{ItemID: sessionruntime.FollowUpItemID(item.ID)}
}

func (e queueEnv) promotable(t *testing.T, proc *discussProcess, invocation string) sessionruntime.FollowUpItem {
	t.Helper()
	item, err := proc.svc.EnqueueFollowUp(context.Background(), e.input(invocation, "promote "+invocation))
	require.NoError(t, err)
	return item
}

// assetRefsOf are the asset links a run's own row writer would make for a
// message's attachments. Only attachments with a content hash are linked.
func assetRefsOf(attachments []turn.Attachment) []messagepkg.AssetRef {
	var refs []messagepkg.AssetRef
	for i, att := range attachments {
		hash := strings.TrimSpace(att.ContentHash)
		if hash == "" {
			continue
		}
		refs = append(refs, messagepkg.AssetRef{
			ContentHash: hash, Role: "attachment", Ordinal: i,
			Mime: strings.TrimSpace(att.Mime), Name: strings.TrimSpace(att.Name),
		})
	}
	return refs
}

// assetsOf names the messages of the session a content hash is linked to.
func (e queueEnv) assetsOf(t *testing.T, contentHash string) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `
		SELECT message.source_message_id
		FROM bot_history_message_assets asset
		JOIN bot_history_messages message ON message.id = asset.message_id
		WHERE message.session_id = $1 AND asset.content_hash = $2
		ORDER BY message.turn_position, message.turn_message_seq
	`, e.sessionID, contentHash)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var external string
		require.NoError(t, rows.Scan(&external))
		out = append(out, external)
	}
	require.NoError(t, rows.Err())
	return out
}

// webQueueTurn is the turn a Web text send stores when it waits.
func (e queueEnv) webQueueTurn(text string) func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error) {
	return func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error) {
		cmd := e.turnCommand("ignored", text)
		cmd.ExternalMessageID = ""
		cmd.IdempotencyKey = ""
		cmd.ReplyTarget, cmd.CurrentChannel, cmd.ConversationType = e.botID, "web", "private"
		return cmd, &auth.RouteCredentials{BotID: e.botID, ChatID: e.botID, ReplyTarget: e.botID, UserID: "user-1"}, nil
	}
}

func (e queueEnv) webSend(invocation, text string) agentapi.WebAdmitInput {
	return agentapi.WebAdmitInput{
		BotID: e.botID, SessionID: e.sessionID, InvocationID: invocation,
		Payload: []byte(`{"kind":"message","text":` + strconvQuote(text) + `}`),
		Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		},
		QueueTurn: e.webQueueTurn(text),
	}
}

func strconvQuote(text string) string {
	raw, _ := json.Marshal(text)
	return string(raw)
}

// drainInBackground consumes a started run's channels, as the queue's own start does for
// the runs it admits, so a run a test starts is not held up by its delivery.
func drainInBackground(handle turn.RunHandle) {
	events, errs := handle.Events(), handle.Errs()
	for events != nil || errs != nil {
		select {
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		}
	}
}
