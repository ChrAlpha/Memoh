package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/agenttest"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/auth"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
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
	proc := newDiscussProcess(t, env.discussEnv, steps...)
	proc.svc.sessionService = chatSessionService(env.botID)
	rewireComponents(proc.svc)
	proc.svc.queue = env.store
	rewireRun(proc.svc)
	setAllowedTeam(proc.svc, team.DefaultTeamID)
	return proc
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
	go drainDeferredTurn(handle)
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

func (e queueEnv) input(invocation, text string) QueueInput {
	return QueueInput{
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

// Follow-ups queued behind a run each start a run of their own when it ends, in
// the order they were queued, and none starts twice.
func TestQueuedFollowUpsStartOneRunEachInOrderWhenTheRunEnds(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("first answer")
	proc := newQueueProcess(t, env, held, &discussStep{text: "second answer"}, &discussStep{text: "third answer"})
	release := startHeldRun(t, proc, env, held)

	for _, in := range [][2]string{{"q1", "queued one"}, {"q2", "queued two"}} {
		_, err := proc.svc.EnqueueFollowUp(context.Background(), env.input(in[0], in[1]))
		require.NoError(t, err)
	}
	items, err := proc.svc.ListSessionQueues(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	require.Len(t, items.FollowUp, 2)

	release()
	runs := env.waitQueuedRuns(t, 2)
	require.NotEqual(t, runs[0].invocation, runs[1].invocation)
	env.waitState(t, "q1", "consumed")
	env.waitState(t, "q2", "consumed")

	texts := modelTexts(t, proc)
	require.Len(t, texts, 3, "the first run and one run per queued follow-up")
	require.Contains(t, texts[1], "queued one")
	require.NotContains(t, texts[1], "queued two")
	require.Contains(t, texts[2], "queued two")
}

func TestEnqueueFollowUpOnAnIdleSessionIsRefused(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env)
	_, err := proc.svc.EnqueueFollowUp(context.Background(), env.input("q1", "nothing runs"))
	require.ErrorIs(t, err, sessionruntime.ErrQueueNoActiveRun)
	require.Empty(t, mustPending(t, proc, env))
}

func mustPending(t *testing.T, proc *discussProcess, env queueEnv) []sessionruntime.FollowUpItem {
	t.Helper()
	items, err := proc.svc.ListSessionQueues(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	return items.FollowUp
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

// Inputs of one sender that waited together are answered by one run, and each
// is still a message of its own: its row has its sender, its mention, the
// message it replies to and its attachments. The run's request is the last of
// them, so the reply quotes it.
func TestQueuedInputsOfOneSenderAreAnsweredByOneRun(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("first answer")
	proc := newQueueProcess(t, env, held, &discussStep{text: "one answer to all three"})
	release := startHeldRun(t, proc, env, held)
	alice := env.participant(t, false)
	env.parkMessage(t, "m1", alice.identityID, "pick up milk", func(c *turn.StartTurnCommand) { c.MentionsBot = true }, time.Unix(100, 0))
	env.parkMessage(t, "m2", alice.identityID, "and eggs", func(c *turn.StartTurnCommand) {
		c.SourceReplyToMessageID = "quoted-7"
		c.Attachments = []turn.Attachment{{Type: "image", Name: "eggs.png", Path: "/workspace/eggs.png", ContentHash: "hash-eggs"}}
	}, time.Unix(200, 0))
	env.parkMessage(t, "m3", alice.identityID, "and bread", nil, time.Unix(300, 0))
	_, err := proc.svc.EnqueueFollowUp(context.Background(), env.input("panel", "typed in the panel"))
	require.NoError(t, err)

	release()
	env.waitQueuedRuns(t, 2)
	texts := modelTexts(t, proc)
	require.Len(t, texts, 3, "the first run, one run for the three messages, one for the panel's text")
	merged := texts[1]
	milk, eggs, bread := strings.Index(merged, "pick up milk"), strings.Index(merged, "and eggs"), strings.Index(merged, "and bread")
	require.True(t, milk >= 0 && milk < eggs && eggs < bread, "every text, in arrival order")
	require.NotContains(t, merged, "typed in the panel", "an input without a merge key is not folded into the batch")
	var asked []string
	for _, message := range proc.provider.recorded()[1].Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			if text, ok := part.(sdk.TextPart); ok {
				asked = append(asked, text.Text)
			}
		}
	}
	require.Len(t, asked, 4, "the first question and the three messages are four user messages, not one with three texts")
	for i, want := range []string{"the first question", "pick up milk", "and eggs", "and bread"} {
		require.Contains(t, asked[i], want)
	}

	require.Contains(t, asked[2], `<attachment path="/workspace/eggs.png"/>`, "an attachment reaches the model with its own message")
	require.NotContains(t, asked[1], "eggs.png")
	require.NotContains(t, asked[3], "eggs.png")

	byExternal := map[string]userRow{}
	var order []string
	for _, row := range env.userRows(t) {
		byExternal[row.external] = row
		order = append(order, row.external)
	}
	mi, ei, bi := indexOf(order, "m1"), indexOf(order, "m2"), indexOf(order, "m3")
	require.True(t, mi >= 0 && mi < ei && ei < bi, "rows in arrival order, last the run's: %v", order)
	require.Equal(t, alice.identityID, byExternal["m1"].sender, "each row keeps its sender")
	require.Equal(t, alice.identityID, byExternal["m3"].sender)
	require.Contains(t, byExternal["m1"].content, `mentions_me="true"`, "the mention stays with its message")
	require.NotContains(t, byExternal["m2"].content, `mentions_me`)
	require.Contains(t, byExternal["m2"].content, `<in-reply-to id="quoted-7"`, "the reply stays with its message")
	require.Equal(t, "quoted-7", byExternal["m2"].reply)
	require.Contains(t, byExternal["m2"].content, `<attachment path="/workspace/eggs.png"/>`, "the attachment stays with its message")
	require.NotContains(t, byExternal["m1"].content, "eggs.png")
	require.Equal(t, []string{"m2"}, env.assetsOf(t, "hash-eggs"), "the attachment is linked to the row of the message it came with")
	require.Equal(t, "pick up milk", byExternal["m1"].display)
	require.Equal(t, int64(1), byExternal["m1"].seq, "each message opens a turn of its own")
	require.Less(t, byExternal["m1"].position, byExternal["m2"].position)
	require.Less(t, byExternal["m2"].position, byExternal["m3"].position)
	for _, id := range []string{"m1", "m2", "m3", "panel"} {
		env.waitState(t, id, "consumed")
	}
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}

// The rows of a batch are written under the identity of the items they waited
// as, once. A batch that is started again after it wrote them, as after a crash
// between the rows and the run, finds them and adds none.
func TestPrecedingRowsOfABatchAreWrittenOnce(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env, &discussStep{text: "answered"})
	ctx := context.Background()
	alice := env.participant(t, false)
	first := env.parkMessage(t, "m1", alice.identityID, "earlier", nil, time.Unix(100, 0))
	env.parkMessage(t, "m2", alice.identityID, "later", nil, time.Unix(200, 0))

	batch, ok, err := env.store.ClaimBatch(ctx, env.key(), sessionruntime.QueueOriginFollowUp)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, batch.Items, 2)
	var inputs []turnwire.QueuedInput
	for _, item := range batch.Items {
		input, err := queuedInput(item)
		require.NoError(t, err)
		input.ItemID = item.ID
		inputs = append(inputs, input)
	}
	merged, err := turnwire.MergeQueued(inputs)
	require.NoError(t, err)
	require.Len(t, merged.Command.Preceding, 1, "the earlier message is one of the turn's own")
	require.Equal(t, first.ID, merged.Command.Preceding[0].ID)

	// The batch's drainer wrote the rows and died before it admitted the run.
	require.NoError(t, proc.svc.persistPrecedingMessages(ctx, merged.Command))
	require.NoError(t, proc.svc.persistPrecedingMessages(ctx, merged.Command))
	var rows int
	require.NoError(t, env.pool.QueryRow(ctx, `
		SELECT count(*) FROM bot_history_messages WHERE session_id = $1 AND source_message_id = 'm1'
	`, env.sessionID).Scan(&rows))
	require.Equal(t, 1, rows, "the earlier message is one row however often its batch was started")
	var id string
	require.NoError(t, env.pool.QueryRow(ctx, `
		SELECT id::text FROM bot_history_messages WHERE session_id = $1 AND source_message_id = 'm1'
	`, env.sessionID).Scan(&id))
	require.Equal(t, first.ID, id, "the row has the identity of its queue item")
}

// userMessageTexts are the user messages of a model request, in order, each as
// the text of its parts.
func userMessageTexts(req sdk.Request) []string {
	var out []string
	for _, message := range req.Messages {
		if message.Role != sdk.MessageRoleUser {
			continue
		}
		var text strings.Builder
		for _, part := range message.Content {
			if p, ok := part.(sdk.TextPart); ok {
				text.WriteString(p.Text)
			}
		}
		out = append(out, text.String())
	}
	return out
}

// What the model reads for a queued batch is what it reads when the same
// messages are recorded as they came and the last one mentions the bot: the
// same user messages, in the same order, with the same headers. What a message
// carried stays with it, its attachments included; only the time of the last
// message is left out of the comparison, because a run builds it when it starts.
func TestQueuedBatchReadsLikeTheSameMessagesRecordedAsTheyCame(t *testing.T) {
	queued := newQueueEnv(t)
	held := heldStep("first answer")
	queuedProc := newQueueProcess(t, queued, held, &discussStep{text: "answered"})
	release := startHeldRun(t, queuedProc, queued, held)
	alice := queued.participant(t, false)
	runbook := []turn.Attachment{{Type: "image", Name: "runbook.png", Path: "/workspace/runbook.png", ContentHash: "hash-runbook"}}
	queued.parkMessage(t, "m1", alice.identityID, "the deploy is at noon", nil, time.Unix(100, 0))
	queued.parkMessage(t, "m2", alice.identityID, "bring the runbook", func(c *turn.StartTurnCommand) {
		c.SourceReplyToMessageID = "quoted-9"
		c.Attachments = runbook
	}, time.Unix(200, 0))
	queued.parkMessage(t, "m3", alice.identityID, "can you check it", func(c *turn.StartTurnCommand) { c.MentionsBot = true }, time.Unix(300, 0))
	release()
	queued.waitQueuedRuns(t, 1)
	requests := queuedProc.provider.recorded()
	require.Len(t, requests, 2)
	// The first request's question belongs to the run that was in flight.
	batch := userMessageTexts(requests[1])[1:]

	passive := newDiscussEnv(t, "private")
	passiveAlice := passive.participant(t, false)
	passiveProc := newDiscussProcess(t, passive, &discussStep{text: "answered"})
	for _, in := range []struct {
		id, text    string
		reply       string
		mention     bool
		at          int64
		attachments []turn.Attachment
	}{
		{"m1", "the deploy is at noon", "", false, 100, nil},
		{"m2", "bring the runbook", "quoted-9", false, 200, runbook},
		{"m3", "can you check it", "", true, 300, nil},
	} {
		passive.writeHeaded(t, passiveAlice, in.id, in.text, in.reply, in.mention, time.Unix(in.at, 0).UTC(), in.attachments)
	}
	outcome, done := passiveProc.admit(t, passive)
	require.Equal(t, DiscussAdmitted, outcome)
	waitDiscussRun(t, done)
	recorded := passiveProc.provider.recorded()
	require.Len(t, recorded, 1)
	asRecorded := userMessageTexts(recorded[0])

	require.Len(t, batch, 3, "the batch's three messages")
	require.Len(t, asRecorded, 3, "the three recorded messages")
	require.Equal(t, normalizeHeaders(asRecorded), normalizeHeaders(batch))
	require.Contains(t, batch[0], `id="m1"`)
	require.Contains(t, batch[1], `<in-reply-to id="quoted-9"`)
	require.Contains(t, batch[2], `mentions_me="true"`)
	require.NotContains(t, batch[0], "mentions_me")
	require.Contains(t, batch[1], `<attachment path="/workspace/runbook.png"/>`, "an attachment stays with the message that carried it")
	require.NotContains(t, batch[0], "runbook.png")
	require.NotContains(t, batch[2], "runbook.png")

	// The attachment is linked to the row of the message it came with, not to
	// the run's request: the earlier messages are rows of the session as any
	// other.
	require.Equal(t, []string{"m2"}, queued.assetsOf(t, "hash-runbook"))
}

// normalizeHeaders drops the time of the last message alone: a run builds it
// when it starts, so two sessions that say the same thing differ there and
// nowhere else. Everything else a message carried is compared as it is.
func normalizeHeaders(messages []string) []string {
	out := append([]string(nil), messages...)
	if len(out) > 0 {
		out[len(out)-1] = headerTime.ReplaceAllString(out[len(out)-1], "")
	}
	return out
}

var headerTime = regexp.MustCompile(` t="[^"]*"`)

// A new turn does not run ahead of input already waiting for the session, and
// a message the queue holds or has taken is recognized when it is delivered
// again.
func TestAnIncomingTurnWaitsBehindQueuedInputAndRepeatsAreRecognized(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env, &discussStep{text: "answered once"})
	ctx := context.Background()
	env.park(t, "queued-msg", "alice", "was here first")

	_, err := proc.svc.StartTurn(ctx, env.turnCommand("late-msg", "arrives while input waits"))
	require.ErrorIs(t, err, turn.ErrSessionBusy, "the session is idle but the queue is not empty")
	_, err = proc.svc.StartTurn(ctx, env.turnCommand("queued-msg", "was here first"))
	require.ErrorIs(t, err, turn.ErrDuplicateTurn, "the message is already in the queue")

	started, err := proc.svc.drainFollowUps(ctx, env.key())
	require.NoError(t, err)
	require.True(t, started)
	env.waitQueuedRuns(t, 1)
	env.waitState(t, "queued-msg", "consumed")

	_, err = proc.svc.StartTurn(ctx, env.turnCommand("queued-msg", "was here first"))
	require.ErrorIs(t, err, turn.ErrDuplicateTurn, "the message was taken, and answered once")
	handle, err := proc.svc.StartTurn(ctx, env.turnCommand("late-msg", "now the queue is empty"))
	require.NoError(t, err, "with the queue empty the turn runs at once")
	go drainDeferredTurn(handle)
}

// An enqueue that saw a run in flight can land after the run ended, and then
// nobody's end of run would start it. The enqueue looks again.
func TestAnInputQueuedAfterTheRunEndedIsStartedByTheEnqueue(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env, &discussStep{text: "answered"})
	// The session's live state still shows the run that has already ended.
	backend := sessionruntime.NewMemoryBackend()
	_, _, err := backend.Update(context.Background(), env.key(), func(snapshot sessionruntime.Snapshot, _ bool) (sessionruntime.Snapshot, bool, error) {
		snapshot.BotID, snapshot.SessionID = env.botID, env.sessionID
		snapshot.CurrentRunView = &sessionruntime.CurrentRunView{RunID: "ended-run", Generation: "g", OwnerID: "o", Status: sessionruntime.RunStatusRunning}
		return snapshot, true, nil
	})
	require.NoError(t, err)
	stale := sessionruntime.NewManager(backend, sessionruntime.Options{})
	t.Cleanup(func() { _ = stale.Close() })
	proc.svc.sessionManager = stale
	rewireRun(proc.svc)

	_, err = proc.svc.EnqueueFollowUp(context.Background(), env.input("late", "queued behind a run that ended"))
	require.NoError(t, err)
	env.waitQueuedRuns(t, 1)
	env.waitState(t, "late", "consumed")
}

// Two servers that look at the same queue at the same moment admit its batch
// once between them.
func TestTwoServersDrainingTheSameQueueStartOneRun(t *testing.T) {
	env := newQueueEnv(t)
	first := newQueueProcess(t, env, &discussStep{text: "answered"}, &discussStep{text: "answered again"})
	second := newQueueProcess(t, env, &discussStep{text: "answered"}, &discussStep{text: "answered again"})
	env.park(t, "m1", "alice", "only one run")

	var wg sync.WaitGroup
	for _, proc := range []*discussProcess{first, second, first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := proc.svc.admitPending(context.Background(), env.botID, env.sessionID)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	env.waitQueuedRuns(t, 1)
	time.Sleep(300 * time.Millisecond)
	require.Len(t, env.queuedRuns(t), 1)
	require.Equal(t, 1, len(first.provider.recorded())+len(second.provider.recorded()), "the model was called once")
}

// settleFailing is a queue whose drainer dies right after admitting the run.
type settleFailing struct {
	queuedInputStore
	fail func() bool
}

func (s *settleFailing) SettleBatch(ctx context.Context, batch sessionruntime.QueueBatch) error {
	if s.fail() {
		return errors.New("the process died before it settled the batch")
	}
	return s.queuedInputStore.SettleBatch(ctx, batch)
}

// A drainer that dies after admitting the run and before recording it leaves
// the batch claimed. When the lease runs out the batch is claimed again under
// the same invocation id, which finds the run and settles; it is not answered
// twice.
func TestBatchWhoseDrainerDiedAfterAdmissionIsSettledAndNotRunTwice(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env, &discussStep{text: "answered"}, &discussStep{text: "answered a second time"})
	short := sessionruntime.NewQueuedInputs(dbsqlc.New(env.pool), env.pool, sessionruntime.WithQueueClaimLease(300*time.Millisecond))
	var dead sync.Mutex
	crashed := true
	proc.svc.queue = &settleFailing{queuedInputStore: short, fail: func() bool { dead.Lock(); defer dead.Unlock(); return crashed }}
	rewireRun(proc.svc)
	env.park(t, "m1", "alice", "answered once")

	_, err := proc.svc.admitPending(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	env.waitQueuedRuns(t, 1)
	require.Equal(t, "claimed", env.stateOf(t, "m1"), "the run exists and the batch was never settled")

	time.Sleep(400 * time.Millisecond)
	dead.Lock()
	crashed = false
	dead.Unlock()
	_, err = proc.svc.admitPending(context.Background(), env.botID, env.sessionID)
	require.NoError(t, err)
	env.waitState(t, "m1", "consumed")
	time.Sleep(200 * time.Millisecond)
	require.Len(t, env.queuedRuns(t), 1, "no second run")
	require.Len(t, proc.provider.recorded(), 1)
}

// A drainer that dies before admitting leaves a batch nobody started. The
// next claim, after the lease, starts it.
func TestBatchWhoseDrainerDiedBeforeAdmissionIsStartedByTheNextClaim(t *testing.T) {
	env := newQueueEnv(t)
	short := sessionruntime.NewQueuedInputs(dbsqlc.New(env.pool), env.pool, sessionruntime.WithQueueClaimLease(300*time.Millisecond))
	proc := newQueueProcess(t, env, &discussStep{text: "answered"})
	proc.svc.queue = short
	rewireRun(proc.svc)
	env.park(t, "m1", "alice", "claimed by a process that died")
	_, ok, err := short.ClaimBatch(context.Background(), env.key(), sessionruntime.QueueOriginFollowUp)
	require.NoError(t, err)
	require.True(t, ok)

	started, err := proc.svc.drainFollowUps(context.Background(), env.key())
	require.NoError(t, err)
	require.False(t, started, "a live claim is another drainer's")
	time.Sleep(400 * time.Millisecond)
	started, err = proc.svc.drainFollowUps(context.Background(), env.key())
	require.NoError(t, err)
	require.True(t, started)
	env.waitQueuedRuns(t, 1)
	env.waitState(t, "m1", "consumed")
}

// An input that cannot be started is rejected and does not hold up the ones
// behind it; so is one of a team this instance does not serve.
func TestUnstartableInputIsRejectedAndDoesNotBlockTheQueue(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env, &discussStep{text: "answered"})
	ctx := context.Background()
	_, err := env.store.Enqueue(ctx, sessionruntime.QueueEnqueue{
		Key: env.key(), Origin: sessionruntime.QueueOriginFollowUp, InvocationID: "legacy",
		Payload: []byte(`{"text":"from an earlier release, with no command"}`),
	})
	require.NoError(t, err)
	foreign := env.turnCommand("foreign", "for another team")
	foreign.TeamID = "00000000-0000-0000-0000-0000000000ff"
	setAllowedTeam(proc.svc, team.DefaultTeamID)
	_, err = env.store.Enqueue(ctx, sessionruntime.QueueEnqueue{
		Key: env.key(), Origin: sessionruntime.QueueOriginFollowUp, InvocationID: "foreign",
		Payload: queuedPayload(t, foreign, time.Now()),
	})
	require.NoError(t, err)
	env.park(t, "fine", "", "this one starts")

	started, err := proc.svc.drainFollowUps(ctx, env.key())
	require.NoError(t, err)
	require.True(t, started, "the drain moves past what it cannot start")
	env.waitQueuedRuns(t, 1)
	require.Equal(t, "cancelled", env.stateOf(t, "legacy"))
	require.Equal(t, "cancelled", env.stateOf(t, "foreign"))
	env.waitState(t, "fine", "consumed")
	var code string
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT error_code FROM session_queued_inputs WHERE session_id = $1 AND invocation_id = 'legacy'`, env.sessionID).Scan(&code))
	require.Equal(t, sessionruntime.QueueErrorFollowUpCommandInvalid, code)
}

// A turn that was queued is started with credentials signed when it starts,
// from the identities stored with it.
func TestQueuedTurnIsSignedWhenItStartsNotWhenItWasQueued(t *testing.T) {
	ctx := context.Background()
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env)
	useSecret(proc.svc, "queue-test-secret")
	cmd := env.turnCommand("m1", "hello")
	payload := queuedPayload(t, cmd, time.Now())
	stored := turnwire.ParseQueuedInput(payload)
	require.Empty(t, stored.Command.Token)
	require.Empty(t, stored.Command.ChatToken)
	require.NotContains(t, string(payload), "queue-test-secret")

	var started turn.StartTurnCommand
	merged, err := turnwire.MergeQueued([]turnwire.QueuedInput{stored})
	require.NoError(t, err)
	started = merged.Command
	proc.svc.signQueuedCommand(ctx, &started, merged.Claims)
	require.NotEmpty(t, started.ChatToken, "a turn that answers somewhere gets a chat token")
	require.NotEmpty(t, started.Token)

	unsigned := merged.Command
	proc.svc.signQueuedCommand(ctx, &unsigned, nil)
	require.Empty(t, unsigned.ChatToken, "a text typed into the queue panel never had credentials")
	unbound := merged.Command
	proc.svc.signQueuedCommand(ctx, &unbound, &auth.RouteCredentials{BotID: cmd.BotID, ChatID: cmd.ChatID, ReplyTarget: cmd.ReplyTarget})
	require.Empty(t, unbound.ChatToken, "a sender no account is bound to starts without, as an inbound turn does")
}

// steerBackend is a live backend whose session has a run in flight that takes
// steers, for the cases that exercise the promotion without a real run.
func steerBackend(t *testing.T, env queueEnv) (*sessionruntime.MemoryBackend, sessionruntime.RunHandle) {
	t.Helper()
	backend := sessionruntime.NewMemoryBackend()
	handle := sessionruntime.RunHandle{
		BotID: env.botID, SessionID: env.sessionID, RunID: "run-x", OwnerID: "owner-x", Generation: "gen-x", FencingToken: 1,
	}
	_, _, err := backend.Update(context.Background(), env.key(), func(snapshot sessionruntime.Snapshot, _ bool) (sessionruntime.Snapshot, bool, error) {
		snapshot.BotID, snapshot.SessionID = env.botID, env.sessionID
		snapshot.CurrentRunView = &sessionruntime.CurrentRunView{
			RunID: handle.RunID, OwnerID: handle.OwnerID, Generation: handle.Generation,
			SteerSupported: true, Status: sessionruntime.RunStatusRunning,
		}
		return snapshot, true, nil
	})
	require.NoError(t, err)
	return backend, handle
}

func useLiveBackend(t *testing.T, proc *discussProcess, backend sessionruntime.Backend) *sessionruntime.Manager {
	t.Helper()
	manager := sessionruntime.NewManager(backend, sessionruntime.Options{})
	t.Cleanup(func() { _ = manager.Close() })
	proc.svc.sessionManager = manager
	rewireRun(proc.svc)
	return manager
}

// steerWriteFails is a live backend that cannot take a steer, as when the
// Redis behind it is down.
type steerWriteFails struct{ *sessionruntime.MemoryBackend }

func (steerWriteFails) EnqueueSteer(context.Context, sessionruntime.Key, string, string, []byte) (sessionruntime.SteerItem, error) {
	return sessionruntime.SteerItem{}, errors.New("the live backend is unavailable")
}

// completeFails is a queue that cannot record a promotion, as when the
// database refuses the write.
type completeFails struct{ queuedInputStore }

func (completeFails) CompletePromotion(context.Context, string, string) error {
	return errors.New("the database refused the write")
}

// forgetfulBackend is a live backend that can be made to lose its steers.
type forgetfulBackend struct {
	*sessionruntime.MemoryBackend
	forget atomic.Bool
}

func (b *forgetfulBackend) LookupSteer(ctx context.Context, key sessionruntime.Key, id sessionruntime.SteerItemID) (sessionruntime.SteerItem, bool, error) {
	if b.forget.Load() {
		return sessionruntime.SteerItem{}, false, nil
	}
	return b.MemoryBackend.LookupSteer(ctx, key, id)
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

// A follow-up promoted to a steer is taken by the run at its next step, and the
// queue records it as consumed. It does not also start a run of its own.
func TestPromotedFollowUpIsAppliedByTheRunAndConsumed(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("first answer")
	proc := newQueueProcess(t, env, held, &discussStep{text: "answer that saw the steer"})
	release := startHeldRun(t, proc, env, held)
	item := env.promotable(t, proc, "p1")

	result, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, sessionruntime.FollowUpPendingRef{ItemID: item.ID})
	require.NoError(t, err)
	require.Equal(t, string(item.ID), string(result.Steer.ID), "the steer carries the input's own id")
	require.Equal(t, "promoted", env.stateOf(t, "p1"))
	steers, err := proc.svc.sessionManager.PendingSteers(context.Background(), env.key(), 0)
	require.NoError(t, err)
	require.Len(t, steers, 1)
	require.Empty(t, mustPending(t, proc, env), "the input has left the follow-up queue")

	again, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, sessionruntime.FollowUpPendingRef{ItemID: item.ID})
	require.NoError(t, err, "promoting again is the same promotion")
	require.Equal(t, result.Steer.ID, again.Steer.ID)

	release()
	env.waitState(t, "p1", "consumed")
	texts := modelTexts(t, proc)
	require.Len(t, texts, 2)
	require.Contains(t, texts[1], "promote p1", "the run's next step carried the steer")
	require.Empty(t, env.queuedRuns(t), "a consumed steer starts no run")
}

// The three places a promotion can fail. Whatever happens, the input is
// delivered, and it is never both in the queue and in the steer queue.
func TestPromotionFailuresLeaveTheInputDeliverableOnce(t *testing.T) {
	steerKey := func(env queueEnv) sessionruntime.Key { return env.key() }

	t.Run("the live write fails: the input stays queued", func(t *testing.T) {
		env := newQueueEnv(t)
		proc := newQueueProcess(t, env)
		backend, _ := steerBackend(t, env)
		useLiveBackend(t, proc, steerWriteFails{backend})
		item := env.parked(t, "a")
		_, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, item)
		require.Error(t, err)
		require.Equal(t, "queued", env.stateOf(t, "a"), "the promotion was put back")
		require.Len(t, mustPending(t, proc, env), 1)
	})

	t.Run("the database write fails after the steer: the steer is live and the sweep records it", func(t *testing.T) {
		env := newQueueEnv(t)
		proc := newQueueProcess(t, env)
		backend, _ := steerBackend(t, env)
		manager := useLiveBackend(t, proc, backend)
		item := env.parked(t, "b")
		proc.svc.queue = completeFails{env.store}
		rewireRun(proc.svc)
		result, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, item)
		require.NoError(t, err, "the steer exists, so the input was promoted")
		require.NotEmpty(t, result.Steer.ID)
		require.Equal(t, "promoting", env.stateOf(t, "b"))
		steers, err := manager.PendingSteers(context.Background(), steerKey(env), 0)
		require.NoError(t, err)
		require.Len(t, steers, 1)

		proc.svc.queue = env.store
		rewireRun(proc.svc)
		proc.svc.reconcileStalePromotions(context.Background(), time.Now().Add(promotionGrace+time.Minute))
		require.Equal(t, "promoted", env.stateOf(t, "b"))
		steers, err = manager.PendingSteers(context.Background(), steerKey(env), 0)
		require.NoError(t, err)
		require.Len(t, steers, 1, "the steer is untouched")
	})

	t.Run("killed after the first step: no steer, the sweep puts the input back", func(t *testing.T) {
		env := newQueueEnv(t)
		proc := newQueueProcess(t, env)
		backend, _ := steerBackend(t, env)
		manager := useLiveBackend(t, proc, backend)
		item := env.parked(t, "c")
		_, err := env.store.BeginPromotion(context.Background(), env.key(), string(item.ItemID))
		require.NoError(t, err)
		require.Equal(t, "promoting", env.stateOf(t, "c"))

		proc.svc.reconcileStalePromotions(context.Background(), time.Now().Add(time.Minute))
		require.Equal(t, "promoting", env.stateOf(t, "c"), "a promotion that is not old is still being made")
		proc.svc.reconcileStalePromotions(context.Background(), time.Now().Add(promotionGrace+time.Minute))
		require.Equal(t, "queued", env.stateOf(t, "c"))
		steers, err := manager.PendingSteers(context.Background(), steerKey(env), 0)
		require.NoError(t, err)
		require.Empty(t, steers)
	})

	t.Run("killed after the second step: the steer is live and the sweep completes the promotion", func(t *testing.T) {
		env := newQueueEnv(t)
		proc := newQueueProcess(t, env)
		backend, _ := steerBackend(t, env)
		manager := useLiveBackend(t, proc, backend)
		item := env.parked(t, "d")
		_, err := env.store.BeginPromotion(context.Background(), env.key(), string(item.ItemID))
		require.NoError(t, err)
		_, err = manager.EnqueueSteer(context.Background(), env.key(), string(item.ItemID), "promote:"+string(item.ItemID), []byte(`{"text":"promote d"}`))
		require.NoError(t, err)

		proc.svc.reconcileStalePromotions(context.Background(), time.Now().Add(promotionGrace+time.Minute))
		require.Equal(t, "promoted", env.stateOf(t, "d"))
		require.Empty(t, mustPending(t, proc, env), "the input is in the steer queue only")
	})

	t.Run("the steer was applied and the process died before the database knew: consumed", func(t *testing.T) {
		env := newQueueEnv(t)
		proc := newQueueProcess(t, env)
		backend, handle := steerBackend(t, env)
		manager := useLiveBackend(t, proc, backend)
		item := env.parked(t, "e")
		_, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, item)
		require.NoError(t, err)
		_, claim, ok, err := manager.ClaimNextSteer(context.Background(), handle, false)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, manager.ApplySteer(context.Background(), env.key(), claim))
		require.Equal(t, "promoted", env.stateOf(t, "e"), "the database was not told")

		proc.svc.reconcileStalePromotions(context.Background(), time.Now().Add(promotionGrace+time.Minute))
		require.Equal(t, "consumed", env.stateOf(t, "e"))
	})

	t.Run("the steer's record is gone: the input runs again, at least once", func(t *testing.T) {
		env := newQueueEnv(t)
		proc := newQueueProcess(t, env, &discussStep{text: "answered"})
		backend, _ := steerBackend(t, env)
		lost := &forgetfulBackend{MemoryBackend: backend}
		useLiveBackend(t, proc, lost)
		item := env.parked(t, "f")
		_, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, item)
		require.NoError(t, err)
		// The live backend lost what it held: a restart of a memory backend, or
		// Redis flushed.
		lost.forget.Store(true)

		proc.svc.reconcileStalePromotions(context.Background(), time.Now().Add(promotionGrace+time.Minute))
		require.Equal(t, "queued", env.stateOf(t, "f"))
		proc.svc.sessionManager = proc.manager
		rewireRun(proc.svc)
		started, err := proc.svc.drainFollowUps(context.Background(), env.key())
		require.NoError(t, err)
		require.True(t, started)
		env.waitState(t, "f", "consumed")
	})
}

// A run that ends before it takes the steer does not lose the input: its end
// closes the steer, the input goes back to the queue, and the next run is its.
func TestPromotedFollowUpThatTheRunNeverTookIsStartedAfterIt(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("never delivered")
	proc := newQueueProcess(t, env, held, &discussStep{text: "answered after the stop"})
	// A stopped run's context lifecycle record is not what this case is about.
	proc.svc.lifecycle = newTestRecorder(&agenttest.RecordingLifecycleStore{})
	rewireRun(proc.svc)
	startHeldRun(t, proc, env, held)
	item := env.promotable(t, proc, "g")
	_, err := proc.svc.PromoteFollowUpToSteer(context.Background(), env.botID, env.sessionID, sessionruntime.FollowUpPendingRef{ItemID: item.ID})
	require.NoError(t, err)

	stopped, err := proc.svc.StopTurn(context.Background(), turn.StopCommand{TeamID: team.DefaultTeamID, BotID: env.botID, ThreadID: env.sessionID})
	require.NoError(t, err)
	require.True(t, stopped)

	env.waitQueuedRuns(t, 1)
	env.waitState(t, "g", "consumed")
	texts := modelTexts(t, proc)
	require.Equal(t, 1, strings.Count(strings.Join(texts[1:], "\n"), "promote g"), "the input was delivered once")
}

// Queued follow-ups and uncovered discuss input share one admission. Taking
// turns, the queue starts first and the discuss batch after its run, however
// many callers ask at once.
func TestQueueAndDiscussAreAdmittedInTurnsQueueFirst(t *testing.T) {
	env := newDiscussEnv(t, "group")
	alice := env.participant(t, false)
	proc := newDiscussProcess(t, env, &discussStep{text: "answer to the queued input"}, &discussStep{send: "answer to the group"})
	proc.svc.queue = sessionruntime.NewQueuedInputs(dbsqlc.New(env.pool), env.pool)
	rewireRun(proc.svc)
	setAllowedTeam(proc.svc, team.DefaultTeamID)
	hw := env.write(t, passiveInput{from: alice, externalID: "g1", text: "someone in the group said this"})
	queued := queueEnv{discussEnv: env, store: proc.svc.queue.(*sessionruntime.QueuedInputs)}
	queued.park(t, "q1", "", "a follow-up typed in the panel")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := proc.svc.CheckAndAdmitDiscuss(context.Background(), env.botID, env.sessionID); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()

	env.waitRunState(t, env.invocation(hw), "completed")
	queued.waitQueuedRuns(t, 1)
	var queuedFirst bool
	require.NoError(t, env.pool.QueryRow(context.Background(), `
		SELECT (SELECT created_at FROM session_runs WHERE session_id = $1 AND invocation_id LIKE 'queued:%')
		     < (SELECT created_at FROM session_runs WHERE session_id = $1 AND invocation_id LIKE 'discuss:%')
	`, env.sessionID).Scan(&queuedFirst))
	require.True(t, queuedFirst, "the queued input's run came before the discuss run")
	require.Len(t, env.runs(t), 1, "one discuss run for the batch")
	require.Len(t, queued.queuedRuns(t), 1)
}

// The sweep starts what the prompt triggers missed, and deletes what was settled
// long ago.
func TestQueueSweepStartsWhatNothingStartedAndPurgesWhatIsOld(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env, &discussStep{text: "answered"})
	ctx := context.Background()
	env.park(t, "m1", "alice", "nobody kicked the drain")

	proc.svc.sweepQueueScope(ctx, team.DefaultTeamID, time.Now())
	env.waitQueuedRuns(t, 1)
	env.waitState(t, "m1", "consumed")

	proc.svc.sweepQueueScope(ctx, team.DefaultTeamID, time.Now())
	require.Equal(t, "consumed", env.stateOf(t, "m1"), "a recent input is kept, so a redelivery is recognized")
	proc.svc.sweepQueueScope(ctx, team.DefaultTeamID, time.Now().Add(queueSettledRetention+time.Hour))
	var rows int
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT count(*) FROM session_queued_inputs WHERE session_id = $1`, env.sessionID).Scan(&rows))
	require.Zero(t, rows)
}

func TestQueueSweepRunsAtStartAndOnEveryTick(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env)
	scopes := &pagedScopes{scopes: []string{"scope-a", "scope-b"}}
	proc.svc.teamScopes = scopes
	sweep := NewQueueSweep(proc.svc)

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweep.run(ctx, ticks, time.Now)
	}()
	waitSweeps := func(want int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for len(scopes.visited()) < want {
			if time.Now().After(deadline) {
				t.Fatalf("scopes visited = %d, want %d", len(scopes.visited()), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitSweeps(2)
	ticks <- time.Now()
	waitSweeps(4)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not end with its context")
	}
}

// claimProbe counts the callers inside the claim at once.
type claimProbe struct {
	queuedInputStore
	inflight, peak atomic.Int32
}

func (p *claimProbe) ClaimBatch(ctx context.Context, key sessionruntime.Key, origin sessionruntime.QueueOrigin) (sessionruntime.QueueBatch, bool, error) {
	now := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		if peak := p.peak.Load(); now <= peak || p.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	return p.queuedInputStore.ClaimBatch(ctx, key, origin)
}

// Whoever asks what waits on a session, the terminal observer, an enqueue, the
// sweep or a wake, takes turns: two callers are never in the admission of one
// session together.
func TestPendingAdmissionOfOneSessionTakesOneCallerAtATime(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env)
	probe := &claimProbe{queuedInputStore: env.store}
	proc.svc.queue = probe
	rewireRun(proc.svc)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := proc.svc.admitPending(context.Background(), env.botID, env.sessionID); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), probe.peak.Load())

	// Another session is not made to wait for it.
	other := newQueueEnv(t)
	probe.peak.Store(0)
	var both sync.WaitGroup
	for _, e := range []queueEnv{env, other} {
		both.Add(1)
		go func() {
			defer both.Done()
			if _, err := proc.svc.admitPending(context.Background(), e.botID, e.sessionID); err != nil {
				t.Error(err)
			}
		}()
	}
	both.Wait()
	require.Equal(t, int32(2), probe.peak.Load())
}

// writeHeaded stores a message the way the inbound path records one nobody is
// answering yet: a user row with its header, and the session event that makes it
// a message the discuss run answers.
func (e discussEnv) writeHeaded(t *testing.T, from discussParticipant, externalID, text, replyTo string, mentionsMe bool, at time.Time, attachments []turn.Attachment) {
	t.Helper()
	ctx := context.Background()
	var paths []string
	for _, att := range attachments {
		if path := strings.TrimSpace(att.Path); path != "" {
			paths = append(paths, path)
		}
	}
	header := turn.FormatUserHeader(turn.UserMessageHeaderInput{
		MessageID:         externalID,
		ChannelIdentityID: from.identityID,
		DisplayName:       "participant",
		Channel:           discussTestPlatform,
		ConversationType:  "private",
		Target:            discussTestTarget,
		AttachmentPaths:   paths,
		Time:              at,
		ReplyToMessageID:  replyTo,
		MentionsMe:        mentionsMe,
	}, text)
	raw, err := json.Marshal(turn.ModelMessage{Role: "user", Content: turn.NewTextContent(header)})
	require.NoError(t, err)
	meta := map[string]any{"route_id": e.routeID, "platform": discussTestPlatform}
	if replyTo != "" {
		meta["reply"] = map[string]any{"message_id": replyTo}
	}
	svc := messagepkg.NewService(slog.New(slog.DiscardHandler), postgresstore.NewQueriesWithPool(e.pool, dbsqlc.New(e.pool)))
	row, err := svc.Persist(ctx, messagepkg.PersistInput{
		BotID: e.botID, SessionID: e.sessionID, Role: "user", Content: raw,
		SenderChannelIdentityID: from.identityID, ExternalMessageID: externalID,
		SourceReplyToMessageID: replyTo, DisplayText: text, Metadata: meta,
		Assets: assetRefsOf(attachments),
	})
	require.NoError(t, err)
	eventData, err := json.Marshal(map[string]any{"message_id": externalID, "mentions_me": mentionsMe})
	require.NoError(t, err)
	eventID := uuid.New()
	_, err = e.pool.Exec(ctx, `
		INSERT INTO bot_session_events (id, bot_id, session_id, event_kind, event_data, external_message_id, received_at_ms)
		VALUES ($1, $2, $3, 'message', $4, $5, $6)
	`, eventID, e.botID, e.sessionID, eventData, externalID, at.UnixMilli())
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE bot_history_messages SET event_id = $2 WHERE id = $1`, row.ID, eventID)
	require.NoError(t, err)
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

func (e queueEnv) webSend(invocation, text string) WebAdmitInput {
	return WebAdmitInput{
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

// A Web send that meets a run in flight is not refused as busy: it waits in the
// session's queue, the client is told so, and it starts a run of its own when
// the run ends, once however often the client repeats it.
func TestWebSendOnABusySessionWaitsInTheQueueAndRunsInItsTurn(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("first answer")
	proc := newQueueProcess(t, env, held, &discussStep{text: "answer to the queued send"})
	release := startHeldRun(t, proc, env, held)
	ctx := context.Background()

	queued, err := proc.svc.AdmitWebRun(ctx, env.webSend("web-1", "typed while it runs"))
	require.NoError(t, err)
	require.True(t, queued.Queued)
	require.False(t, queued.Duplicate)
	require.NotEmpty(t, queued.QueuedItemID)
	require.Empty(t, queued.RunID, "no run exists yet")
	require.Equal(t, "queued", env.stateOf(t, "web-1"))

	again, err := proc.svc.AdmitWebRun(ctx, env.webSend("web-1", "typed while it runs"))
	require.NoError(t, err)
	require.True(t, again.Queued && again.Duplicate, "a repeated send is answered as the repeat it is")
	require.Empty(t, again.QueuedItemID)
	var rows int
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT count(*) FROM session_queued_inputs WHERE session_id = $1 AND invocation_id = 'web-1'`, env.sessionID).Scan(&rows))
	require.Equal(t, 1, rows, "the repeat added nothing")

	release()
	env.waitQueuedRuns(t, 1)
	env.waitState(t, "web-1", "consumed")
	texts := modelTexts(t, proc)
	require.Len(t, texts, 2)
	require.Contains(t, texts[1], "typed while it runs")

	repeat, err := proc.svc.AdmitWebRun(ctx, env.webSend("web-1", "typed while it runs"))
	require.NoError(t, err)
	require.True(t, repeat.Queued && repeat.Duplicate, "a send the queue has taken is answered as the repeat it is")
}

// A send that would pass input already waiting joins the queue behind it, even
// when no run is in flight.
func TestWebSendBehindQueuedInputWaitsItsTurn(t *testing.T) {
	env := newQueueEnv(t)
	proc := newQueueProcess(t, env)
	env.park(t, "waiting", "alice", "was here first")

	queued, err := proc.svc.AdmitWebRun(context.Background(), env.webSend("web-2", "typed after"))
	require.NoError(t, err)
	require.True(t, queued.Queued, "the session is idle but the queue is not empty")
	require.Equal(t, "queued", env.stateOf(t, "waiting"))
}

// A send the queue cannot replay as text is refused as busy, as it always was.
func TestWebSendTheQueueCannotReplayIsRefusedAsBusy(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("first answer")
	proc := newQueueProcess(t, env, held)
	release := startHeldRun(t, proc, env, held)
	defer release()
	in := env.webSend("web-3", "with a skill")
	in.QueueTurn = nil

	_, err := proc.svc.AdmitWebRun(context.Background(), in)
	require.ErrorIs(t, err, sessionruntime.ErrSessionBusy)
	var rows int
	require.NoError(t, env.pool.QueryRow(context.Background(), `SELECT count(*) FROM session_queued_inputs WHERE session_id = $1`, env.sessionID).Scan(&rows))
	require.Zero(t, rows)
}

// A full queue says so, by the code the client shows, and stores nothing.
func TestWebSendOnAFullQueueIsRefusedWithTheQueueFullCode(t *testing.T) {
	env := newQueueEnv(t)
	held := heldStep("first answer")
	proc := newQueueProcess(t, env, held)
	release := startHeldRun(t, proc, env, held)
	defer release()
	for i := range sessionruntime.MaxPendingQueueItems {
		env.park(t, "filler-"+strconv.Itoa(i), "", "filler")
	}

	_, err := proc.svc.AdmitWebRun(context.Background(), env.webSend("web-4", "one too many"))
	require.Equal(t, apperror.CodeSessionQueueFull, apperror.CodeOf(err))
}
