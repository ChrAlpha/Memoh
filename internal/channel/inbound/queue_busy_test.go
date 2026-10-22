package inbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/identities"
	"github.com/felinics/memoh/internal/channel/route"
)

// memoryBusyQueue is the busy queue as the processor sees it. The session is
// free or taken at the test's say-so, the way the claim finds it in the
// database: a batch is handed out only when no run holds the session.
type memoryBusyQueue struct {
	mu       sync.Mutex
	items    []*busyItem
	taken    bool
	full     bool
	conflict bool
	next     int
	batches  int
	lists    int
	settled  []string
	released []string
}

type busyItem struct {
	id, botID, sessionID, invocation, mergeKey string
	origin                                     BusyOrigin
	payload                                    []byte
	receivedAt                                 time.Time
	state                                      string
	batch                                      string
}

func (q *memoryBusyQueue) Enqueue(_ context.Context, in BusyInput) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.conflict:
		return ErrBusyQueueConflict
	case q.full:
		return ErrBusyQueueFull
	}
	q.addLocked(in)
	return nil
}

func (q *memoryBusyQueue) addLocked(in BusyInput) {
	q.next++
	q.items = append(q.items, &busyItem{
		id: fmt.Sprintf("item-%d", q.next), botID: in.BotID, sessionID: in.SessionID, origin: in.Origin,
		invocation: in.InvocationID, mergeKey: in.MergeKey, payload: in.Payload, receivedAt: in.ReceivedAt, state: "queued",
	})
}

// park stores an input as an earlier enqueue would have.
func (q *memoryBusyQueue) park(in BusyInput) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.addLocked(in)
}

func (q *memoryBusyQueue) setTaken(taken bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.taken = taken
}

func (q *memoryBusyQueue) ClaimBatch(_ context.Context, botID, sessionID string) (BusyBatch, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.taken {
		return BusyBatch{}, false, nil
	}
	var picked []*busyItem
	for _, item := range q.items {
		if item.state != "queued" || item.origin != BusyOriginPlatform || item.sessionID != sessionID {
			continue
		}
		if len(picked) > 0 && (item.mergeKey == "" || item.mergeKey != picked[0].mergeKey) {
			break
		}
		picked = append(picked, item)
		if item.mergeKey == "" {
			break
		}
	}
	if len(picked) == 0 {
		return BusyBatch{}, false, nil
	}
	q.batches++
	batch := BusyBatch{ID: fmt.Sprintf("batch-%d", q.batches), ClaimToken: "token", BotID: botID, SessionID: sessionID}
	for _, item := range picked {
		item.state, item.batch = "claimed", batch.ID
		batch.Inputs = append(batch.Inputs, BusyBatchInput{ID: item.id, Payload: item.payload, ReceivedAt: item.receivedAt})
	}
	return batch, true, nil
}

func (q *memoryBusyQueue) SettleBatch(_ context.Context, batch BusyBatch) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.settled = append(q.settled, batch.ID)
	for _, item := range q.items {
		if item.batch == batch.ID && item.state == "claimed" {
			item.state = "consumed"
		}
	}
	return nil
}

func (q *memoryBusyQueue) ReleaseBatch(_ context.Context, batch BusyBatch) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, batch.ID)
	for _, item := range q.items {
		if item.batch == batch.ID && item.state == "claimed" {
			item.state, item.batch = "queued", ""
		}
	}
	return nil
}

func (q *memoryBusyQueue) Reject(_ context.Context, inputID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, item := range q.items {
		if item.id == inputID {
			item.state = "rejected"
		}
	}
	return nil
}

func (q *memoryBusyQueue) Drainable(context.Context) ([]BusySession, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lists++
	if q.taken {
		return nil, nil
	}
	var out []BusySession
	for _, item := range q.items {
		if item.state == "queued" && item.origin == BusyOriginPlatform {
			out = append(out, BusySession{BotID: item.botID, SessionID: item.sessionID})
			break
		}
	}
	return out, nil
}

func (q *memoryBusyQueue) stateOf(id string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, item := range q.items {
		if item.id == id {
			return item.state
		}
	}
	return ""
}

func (q *memoryBusyQueue) stored() []busyItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]busyItem, len(q.items))
	for i, item := range q.items {
		out[i] = *item
	}
	return out
}

func (q *memoryBusyQueue) settledBatches() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.settled...)
}

func (q *memoryBusyQueue) releasedBatches() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.released...)
}

// replySurface is where the replies of queued messages are delivered: a
// sender that records what reaches it, handed out the way the channel manager
// hands out the surface of an inbound message.
type replySurface struct {
	*fakeReplySender
	openMu sync.Mutex
	opened []string
	err    error
}

func (r *replySurface) Surface(_ context.Context, botID string, _ channel.ChannelType) (channel.StreamReplySender, error) {
	r.openMu.Lock()
	defer r.openMu.Unlock()
	r.opened = append(r.opened, botID)
	if r.err != nil {
		return nil, r.err
	}
	return r.fakeReplySender, nil
}

func (r *replySurface) openedFor() []string {
	r.openMu.Lock()
	defer r.openMu.Unlock()
	return append([]string(nil), r.opened...)
}

func (r *replySurface) streamEvents() []channel.StreamEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channel.StreamEvent(nil), r.events...)
}

func (r *replySurface) delivered() []channel.OutboundMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channel.OutboundMessage(nil), r.sent...)
}

func (r *replySurface) reacted() []channel.ReactRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]channel.ReactRequest(nil), r.reactions...)
}

type recordingWaker struct {
	mu   sync.Mutex
	woke []string
}

func (w *recordingWaker) Wake(_ context.Context, teamID, botID, sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woke = append(w.woke, teamID+"/"+botID+"/"+sessionID)
}

func (w *recordingWaker) wakes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.woke...)
}

// busyGateway is a turn service whose session is taken until the test frees it,
// and that answers every turn it does start with one fixed reply.
type busyGateway struct {
	fakeChatGateway
	mu    sync.Mutex
	taken bool
	cmds  []turn.StartTurnCommand
}

func (g *busyGateway) StartTurn(ctx context.Context, cmd turn.StartTurnCommand) (turn.RunHandle, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cmds = append(g.cmds, cmd)
	if g.taken {
		return nil, turn.ErrSessionBusy
	}
	return g.fakeChatGateway.StartTurn(ctx, cmd)
}

func (g *busyGateway) setTaken(taken bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.taken = taken
}

// attempts are the turns that were offered to the session, whether or not it
// took them.
func (g *busyGateway) attempts() []turn.StartTurnCommand {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]turn.StartTurnCommand(nil), g.cmds...)
}

// started are the turns the queue started: only the queue marks its turns so a
// busy session does not park them again.
func (g *busyGateway) started() []turn.StartTurnCommand {
	var out []turn.StartTurnCommand
	for _, cmd := range g.attempts() {
		if cmd.Origin == turn.OriginPlatformQueue {
			out = append(out, cmd)
		}
	}
	return out
}

type capsAdapter struct {
	typ  channel.ChannelType
	caps channel.ChannelCapabilities
}

func (a *capsAdapter) Type() channel.ChannelType { return a.typ }
func (a *capsAdapter) Descriptor() channel.Descriptor {
	return channel.Descriptor{Type: a.typ, Capabilities: a.caps}
}

type busyHarness struct {
	processor   *ChannelInboundProcessor
	gateway     *busyGateway
	queue       *memoryBusyQueue
	replies     *replySurface
	sender      *fakeReplySender
	waker       *recordingWaker
	channelType channel.ChannelType
	cfg         channel.ChannelConfig
}

const busyReply = "one answer to all of it"

// reactionCaps is a channel that can show a message is waiting and take the
// mark off again: it reacts, and removes a reaction by the message it sits on.
var reactionCaps = channel.ChannelCapabilities{Text: true, Reply: true, Reactions: true, ReactionRemoveKey: channel.ReactionRemoveByMessageID}

// newBusyHarness is a processor whose session is taken, with the queue behind
// it. Messages it receives wait.
func newBusyHarness(t *testing.T, channelType channel.ChannelType, caps channel.ChannelCapabilities) *busyHarness {
	t.Helper()
	registry := channel.NewRegistry()
	registry.MustRegister(&capsAdapter{typ: channelType, caps: caps})
	identitySvc := &fakeChannelIdentityService{
		channelIdentity: identities.ChannelIdentity{ID: "identity-1"},
		linkedUserIDs:   map[string][]string{"identity-1": {"user-1"}},
	}
	chatSvc := &fakeChatService{resolveResult: route.ResolveConversationResult{BotID: "bot-busy", RouteID: "route-busy"}}
	gateway := &busyGateway{taken: true}
	gateway.resp = fakeChatResponse{Messages: []turn.ModelMessage{{Role: "assistant", Content: turn.NewTextContent(busyReply)}}}
	processor := NewChannelInboundProcessor(slog.New(slog.DiscardHandler), registry, chatSvc, chatSvc, gateway, identitySvc,
		&fakePolicyService{ownerUserID: "owner-1"}, "test-secret", time.Minute)
	processor.SetACLService(&fakeChatACL{allowed: true})
	processor.SetSessionEnsurer(&fakeSessionEnsurer{activeSession: SessionResult{ID: "session-busy"}})
	queue := &memoryBusyQueue{taken: true}
	replies := &replySurface{fakeReplySender: &fakeReplySender{}}
	processor.SetBusyQueue(queue, replies)
	waker := &recordingWaker{}
	processor.SetDiscussWaker(waker)
	return &busyHarness{
		processor: processor, gateway: gateway, queue: queue, replies: replies, sender: &fakeReplySender{}, waker: waker,
		channelType: channelType,
		cfg:         channel.ChannelConfig{TeamID: "team-test", BotID: "bot-busy", ChannelType: channelType},
	}
}

func (h *busyHarness) message(id, text string, receivedAt time.Time) channel.InboundMessage {
	return channel.InboundMessage{
		BotID: "bot-busy", Channel: h.channelType, ReplyTarget: "target-busy", ReceivedAt: receivedAt,
		Message: channel.Message{ID: id, Text: text}, Sender: channel.Identity{SubjectID: "ext-busy"},
		Conversation: channel.Conversation{ID: "chat-busy", Type: channel.ConversationTypePrivate},
	}
}

func (h *busyHarness) handle(t *testing.T, msg channel.InboundMessage) {
	t.Helper()
	require.NoError(t, h.processor.HandleInbound(context.Background(), h.cfg, msg, h.sender))
}

// free ends the run the session was taken by.
func (h *busyHarness) free() {
	h.gateway.setTaken(false)
	h.queue.setTaken(false)
}

func (h *busyHarness) kick() {
	h.processor.kickBusyQueue(context.Background(), "bot-busy", "session-busy")
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func reactedMessages(reactions []channel.ReactRequest, remove bool) []string {
	var out []string
	for _, r := range reactions {
		if r.Remove == remove && r.Emoji == queuedReaction {
			out = append(out, r.MessageID)
		}
	}
	return out
}

func validBusyInput(t *testing.T, id, text, mergeKey string) BusyInput {
	t.Helper()
	cmd := turn.StartTurnCommand{
		SchemaVersion: turnwire.StartTurnVersion, TeamID: "team-test", Mode: turn.ModeChat,
		BotID: "bot-busy", ChatID: "bot-busy", ThreadID: "session-busy",
		Query: text, UserVisibleText: text, CurrentChannel: "testchat",
		ReplyTarget: "target-busy", ExternalMessageID: id, IdempotencyKey: "testchat:route-busy:" + id,
	}
	payload, err := turnwire.NewQueuedInput(cmd, nil).Marshal()
	require.NoError(t, err)
	return BusyInput{
		BotID: "bot-busy", SessionID: "session-busy", Origin: BusyOriginPlatform,
		InvocationID: cmd.IdempotencyKey, MergeKey: mergeKey, Payload: payload,
		ReceivedAt: time.Unix(50, 0).UTC(),
	}
}

// A message that meets a busy session is not answered with a hint and not
// dropped. It waits with a mark on it; when the session is free the messages of
// the same sender are answered by one run, the reply quotes the last of them,
// and the marks come off.
func TestBusyPlatformMessagesWaitAndAreAnsweredByOneRunWhenTheSessionIsFree(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	first := h.message("msg-1", "buy milk", time.Unix(100, 0).UTC())
	second := h.message("msg-2", "and eggs", time.Unix(200, 0).UTC())

	h.handle(t, first)
	h.handle(t, second)

	stored := h.queue.stored()
	require.Len(t, stored, 2)
	for _, item := range stored {
		require.Equal(t, BusyOriginPlatform, item.origin)
		require.Equal(t, "queued", item.state)
		require.Equal(t, "identity-1", item.mergeKey, "one sender's messages may be answered together")
	}
	require.Equal(t, []string{"msg-1", "msg-2"}, reactedMessages(h.sender.reactions, false), "each waiting message shows it waits")
	require.Empty(t, h.sender.sent, "no hint and no reply while it waits")
	require.Empty(t, h.replies.delivered())
	for i, want := range []time.Time{time.Unix(100, 0), time.Unix(200, 0)} {
		require.True(t, stored[i].receivedAt.Equal(want), "the arrival time is kept for the reply window")
		input := turnwire.ParseQueuedInput(stored[i].payload)
		cmd, err := input.StartCommand()
		require.NoError(t, err)
		require.Empty(t, cmd.Token, "the queue holds no credentials")
		require.Empty(t, cmd.ChatToken)
		require.NotNil(t, input.Claims)
		require.Equal(t, "identity-1", input.Claims.ChannelIdentityID)
		require.Equal(t, "user-1", input.Claims.UserID)
	}

	h.free()
	h.kick()
	waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })

	started := h.gateway.started()
	require.Len(t, started, 1, "one run answers both")
	cmd := started[0]
	require.Equal(t, "and eggs", cmd.Query, "the run's own message is the last one's alone")
	require.Equal(t, "msg-2", cmd.ExternalMessageID, "the run is the last message's")
	require.Len(t, cmd.Preceding, 1, "the earlier message stays a message of its own")
	require.Equal(t, "buy milk", cmd.Preceding[0].Query)
	require.Equal(t, "msg-1", cmd.Preceding[0].ExternalMessageID)
	require.Equal(t, "item-1", cmd.Preceding[0].ID, "its row has the identity of the queue item")
	require.Equal(t, time.Unix(100, 0).UnixMilli(), cmd.Preceding[0].ReceivedAtMs)
	require.Equal(t, "queued:batch-1", cmd.IdempotencyKey, "the batch is the retry identity")
	require.NotEmpty(t, cmd.ChatToken, "a turn from the queue is signed when it starts")
	require.NotEmpty(t, cmd.Token)

	// The reply is shown the way an inbound message's reply is: through the
	// channel's preview, opened on the last message of the batch, with the
	// capabilities the channel declares.
	require.Equal(t, []string{"bot-busy"}, h.replies.openedFor())
	opened := h.replies.openedWith()
	require.Len(t, opened, 1)
	require.Equal(t, "msg-2", opened[0].SourceMessageID, "the preview hangs under the last message of the batch")
	require.Equal(t, "target-busy", opened[0].Reply.Target)
	var finals int
	for _, event := range h.replies.streamEvents() {
		if event.Type == channel.StreamEventFinal {
			finals++
		}
	}
	require.Equal(t, 1, finals)

	reply := h.replies.delivered()[0]
	require.Equal(t, "target-busy", reply.Target)
	require.Equal(t, busyReply, reply.Message.PlainText())
	require.NotNil(t, reply.Message.Reply)
	require.Equal(t, "msg-2", reply.Message.Reply.MessageID, "the reply quotes the last message")

	require.ElementsMatch(t, []string{"msg-1", "msg-2"}, reactedMessages(h.replies.reacted(), true), "the marks come off")
	require.Equal(t, []string{"batch-1"}, h.queue.settledBatches())
	require.Equal(t, "consumed", h.queue.stateOf("item-1"))
	require.Equal(t, "consumed", h.queue.stateOf("item-2"))
}

// A channel that cannot take a mark off again is not given one: it would stay
// on the message after the reply. The message still waits.
func TestBusyMessageGetsNoMarkWhereItCannotBeTakenOff(t *testing.T) {
	for name, caps := range map[string]channel.ChannelCapabilities{
		"no reactions":           {Text: true, Reply: true},
		"removed by reaction id": {Text: true, Reply: true, Reactions: true, ReactionRemoveKey: channel.ReactionRemoveByReactionID},
	} {
		t.Run(name, func(t *testing.T) {
			h := newBusyHarness(t, "testchat", caps)
			h.handle(t, h.message("msg-1", "hello", time.Unix(100, 0).UTC()))
			require.Len(t, h.queue.stored(), 1)
			require.Empty(t, h.sender.reactions)
			require.Empty(t, h.sender.sent)

			h.free()
			h.kick()
			waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })
			require.Empty(t, h.replies.reacted())
		})
	}
}

func TestMessagesOfDifferentSendersAreAnsweredByRunsOfTheirOwn(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.handle(t, h.message("msg-1", "from the first", time.Unix(100, 0).UTC()))
	other := h.message("msg-2", "from the second", time.Unix(200, 0).UTC())
	other.Sender = channel.Identity{SubjectID: "ext-other"}
	h.processor.identity = NewIdentityResolver(slog.New(slog.DiscardHandler), nil, &fakeChannelIdentityService{
		channelIdentity: identities.ChannelIdentity{ID: "identity-2"},
	}, &fakePolicyService{}, "")
	h.handle(t, other)

	h.free()
	h.kick()
	waitFor(t, "both replies", func() bool { return len(h.replies.delivered()) == 2 })
	started := h.gateway.started()
	require.Len(t, started, 2)
	require.Equal(t, "from the first", started[0].Query)
	require.Equal(t, "from the second", started[1].Query)
}

// A queue that is full says so to the sender, with the code a client can show,
// and stores nothing.
func TestMessageThatMeetsAFullQueueIsRefusedWithTheQueueFullCode(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.queue.full = true
	h.handle(t, h.message("msg-1", "one too many", time.Unix(100, 0).UTC()))

	require.Empty(t, h.queue.stored())
	require.Empty(t, h.sender.reactions)
	var codes []string
	for _, event := range h.sender.events {
		if event.Type == channel.StreamEventError {
			codes = append(codes, event.ErrorCode)
		}
	}
	require.Equal(t, []string{"session.queue_full"}, codes)
}

// A message the platform delivers twice, which the queue already holds, is not
// an error and is not marked or queued again.
func TestRedeliveredMessageTheQueueAlreadyHoldsIsDroppedQuietly(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.queue.conflict = true
	h.handle(t, h.message("msg-1", "again", time.Unix(100, 0).UTC()))

	require.Empty(t, h.queue.stored())
	require.Empty(t, h.sender.reactions)
	for _, event := range h.sender.events {
		require.NotEqual(t, channel.StreamEventError, event.Type)
	}
}

// A local channel's follow-up is started by the server, which delivers the reply
// through the session's subscription. This process only tells the server.
func TestBusyLocalChannelInputIsQueuedForTheServer(t *testing.T) {
	h := newBusyHarness(t, "web", channel.ChannelCapabilities{Text: true})
	h.handle(t, h.message("msg-1", "queued ordinary message", time.Unix(100, 0).UTC()))

	stored := h.queue.stored()
	require.Len(t, stored, 1)
	require.Equal(t, BusyOriginFollowUp, stored[0].origin)
	require.Equal(t, []string{"team-test/bot-busy/session-busy"}, h.waker.wakes())
	cmd, err := turnwire.ParseQueuedInput(stored[0].payload).StartCommand()
	require.NoError(t, err)
	require.Equal(t, "queued ordinary message", cmd.Query)
	require.Equal(t, "msg-1", cmd.ExternalMessageID)
	require.Equal(t, "session-busy", cmd.ThreadID)
	require.NotEmpty(t, cmd.IdempotencyKey)
	require.Empty(t, h.sender.sent)
	require.Empty(t, h.sender.reactions, "the panel shows its own queue")
	require.Empty(t, h.replies.delivered())
}

// A batch the session cannot take after all goes back to the queue with its
// marks on, and is answered once the session is free.
func TestBatchTheSessionTookInTheMeantimeIsReleasedAndAnsweredLater(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.handle(t, h.message("msg-1", "wait for me", time.Unix(100, 0).UTC()))

	// The claim finds no run, and the run that began a moment later wins the
	// session.
	h.queue.setTaken(false)
	h.kick()
	// The drain the park started may have made the claim as well; either way
	// the batch ends up released and waiting.
	waitFor(t, "the release", func() bool {
		return len(h.queue.releasedBatches()) >= 1 && h.queue.stateOf("item-1") == "queued"
	})
	require.Empty(t, h.queue.settledBatches())
	require.Empty(t, h.replies.reacted(), "the message still waits, so it keeps its mark")
	require.Empty(t, h.replies.delivered())

	h.gateway.setTaken(false)
	h.kick()
	waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })
	require.Equal(t, []string{"msg-1"}, reactedMessages(h.replies.reacted(), true))
}

// An input nobody can start does not hold up the ones behind it.
func TestUnstartableInputIsRejectedAndTheNextBatchStarts(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.queue.park(BusyInput{BotID: "bot-busy", SessionID: "session-busy", Origin: BusyOriginPlatform, InvocationID: "garbage", Payload: []byte("not json")})
	h.queue.park(validBusyInput(t, "v1", "a fine message", ""))
	h.free()
	h.kick()

	waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })
	require.Equal(t, "rejected", h.queue.stateOf("item-1"))
	require.Equal(t, "consumed", h.queue.stateOf("item-2"))
	started := h.gateway.started()
	require.Len(t, started, 1)
	require.Equal(t, "a fine message", started[0].Query)
}

// The run a message was answered through ends in this process and starts what
// waited for the session.
func TestEndOfARunThisProcessConsumedStartsWhatWaited(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.queue.park(validBusyInput(t, "v1", "waited behind the run", ""))
	h.free()

	h.handle(t, h.message("msg-live", "a message the session takes at once", time.Unix(300, 0).UTC()))
	waitFor(t, "the queued message's reply", func() bool { return len(h.replies.delivered()) == 1 })
	require.Equal(t, "consumed", h.queue.stateOf("item-1"))
}

// The sweep starts what no trigger did: here, input parked by another process
// while this one saw no run end.
func TestSweepStartsWhatWaitsForAFreeSession(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.queue.park(validBusyInput(t, "v1", "parked elsewhere", ""))
	sweep := NewBusyQueueSweep(h.processor)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweep.run(ctx, ticks)
	}()

	waitFor(t, "a first sweep", func() bool { return h.queue.listCount() > 0 })
	require.Empty(t, h.replies.delivered(), "the session is taken")
	h.free()
	ticks <- time.Now()
	waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not end with its context")
	}
}

func (q *memoryBusyQueue) listCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lists
}

// A drain that is running when another is asked for goes round once more, so a
// trigger is not lost to the moment the drain decides it is done, and two
// drains of one session never run together.
func TestDrainCoalescerRunsOneDrainPerSessionAndOneMoreWhenAskedMeanwhile(t *testing.T) {
	var c drainCoalescer
	var running, maxRunning, rounds atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	drain := func() {
		now := running.Add(1)
		for {
			if cur := maxRunning.Load(); now <= cur || maxRunning.CompareAndSwap(cur, now) {
				break
			}
		}
		if rounds.Add(1) == 1 {
			close(entered)
			<-release
		}
		running.Add(-1)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.run("session", drain)
	}()
	<-entered
	for range 5 {
		c.run("session", drain) // returns at once: the running drain goes round again
	}
	close(release)
	wg.Wait()

	require.Equal(t, int32(2), rounds.Load(), "five asks while it ran make one more round")
	require.Equal(t, int32(1), maxRunning.Load())

	// Another session is not held up by it.
	var other atomic.Bool
	c.run("another", func() { other.Store(true) })
	require.True(t, other.Load())
	require.Empty(t, c.active, "nothing is left behind")
}

// A batch whose channel has no surface now, such as a connection that is down,
// is not started with nowhere to answer: it goes back to the queue and is
// answered when the channel is back.
func TestBatchWhoseChannelHasNoSurfaceWaitsForIt(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.handle(t, h.message("msg-1", "hello", time.Unix(100, 0).UTC()))
	h.replies.err = errors.New("channel connection is down")

	h.free()
	h.kick()
	waitFor(t, "the release", func() bool { return len(h.queue.releasedBatches()) >= 1 })
	require.Empty(t, h.gateway.started(), "no run was started")
	require.Equal(t, "queued", h.queue.stateOf("item-1"))

	h.replies.openMu.Lock()
	h.replies.err = nil
	h.replies.openMu.Unlock()
	h.kick()
	waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })
	require.Equal(t, "consumed", h.queue.stateOf("item-1"))
}

// A platform message whose reply window has closed by the time its run answers
// is not replied to: the channel does not accept a reply to a message it
// received that long ago, and the run is not held back for it. The window is
// judged from when the message arrived, not from when the queue started it.
func TestBatchWhoseReplyWindowHasClosedIsAnsweredWithoutAReply(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.replies.replyWindow = time.Minute
	h.handle(t, h.message("msg-1", "hello", time.Now().Add(-time.Hour).UTC()))

	h.free()
	h.kick()
	waitFor(t, "the batch to settle", func() bool { return h.queue.stateOf("item-1") == "consumed" })
	require.Empty(t, h.replies.delivered(), "a reply the channel no longer accepts is not sent")
	require.Len(t, h.gateway.started(), 1, "the run still answered the message")
}

// An input whose arrival time nobody recorded, as one moved over from Redis,
// has no reply window to answer in: it is judged closed, not from the time it
// happened to be written.
func TestBatchWithoutAnArrivalTimeIsAnsweredWithoutAReply(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.replies.replyWindow = time.Hour
	parked := validBusyInput(t, "msg-1", "hello", "identity-1")
	parked.ReceivedAt = time.Time{}
	h.queue.park(parked)

	h.free()
	h.kick()
	waitFor(t, "the batch to settle", func() bool { return h.queue.stateOf("item-1") == "consumed" })
	require.Empty(t, h.replies.delivered(), "an unknown arrival time is outside the reply window")
	require.Len(t, h.gateway.started(), 1, "the run still answered the message")
}

// A platform message whose reply window is still open is replied to, and the
// reply answers the message it was to: the window is judged from when that
// message arrived, which is what the queue kept for it.
func TestBatchWhoseReplyWindowIsOpenIsAnsweredWithAReply(t *testing.T) {
	h := newBusyHarness(t, "testchat", reactionCaps)
	h.replies.replyWindow = time.Hour
	h.handle(t, h.message("msg-1", "hello", time.Now().UTC()))

	h.free()
	h.kick()
	waitFor(t, "the reply", func() bool { return len(h.replies.delivered()) == 1 })
	opened := h.replies.openedWith()
	require.NotEmpty(t, opened)
	require.Equal(t, "msg-1", opened[0].SourceMessageID, "the reply is addressed to the message it answers")
}
