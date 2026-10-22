package inbound

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/channel"
)

// BusyOrigin says which process starts an input the queue holds.
type BusyOrigin string

const (
	// BusyOriginFollowUp is input of a local channel (web, cli). The server
	// starts it, and the reply reaches the user through the session's
	// subscription.
	BusyOriginFollowUp BusyOrigin = "follow_up"
	// BusyOriginPlatform is a message of a platform channel. This process
	// starts it, because this process delivers the reply.
	BusyOriginPlatform BusyOrigin = "platform_busy"
)

// ErrBusyQueueFull reports a session that already has as many inputs waiting as
// the queue allows.
var ErrBusyQueueFull = errors.New("inbound: the session queue is full")

// ErrBusyQueueConflict reports an input whose retry identity the queue already
// holds with different content.
var ErrBusyQueueConflict = errors.New("inbound: the session queue holds another input under this identity")

// BusyInput is one input to park behind a busy session.
type BusyInput struct {
	BotID, SessionID string
	Origin           BusyOrigin
	// InvocationID is the retry identity of the input: a message delivered
	// again finds the first one.
	InvocationID string
	// MergeKey groups the inputs one run may answer together; empty stands
	// alone.
	MergeKey string
	Payload  []byte
	// ReceivedAt is when the platform delivered the message. The reply window
	// is judged from it.
	ReceivedAt time.Time
}

// BusyBatch is the inputs one run answers, taken from the queue by a claim.
type BusyBatch struct {
	ID               string
	ClaimToken       string
	BotID, SessionID string
	// Recovered marks a batch whose earlier claim ran out.
	Recovered bool
	Inputs    []BusyBatchInput
}

// BusyBatchInput is one stored input of a batch.
type BusyBatchInput struct {
	ID      string
	Payload []byte
	// ReceivedAt is when the platform delivered the message; zero is unknown.
	ReceivedAt time.Time
}

// BusySession names a session with input waiting for this process.
type BusySession struct{ BotID, SessionID string }

// BusyQueue is where a message that met a busy session waits. It is the
// channel boundary's view of the session input queue; the platform messages in
// it are started by this process.
type BusyQueue interface {
	Enqueue(context.Context, BusyInput) error
	// ClaimBatch takes the next batch of platform messages of the session when
	// the session is free.
	ClaimBatch(ctx context.Context, botID, sessionID string) (BusyBatch, bool, error)
	SettleBatch(context.Context, BusyBatch) error
	ReleaseBatch(context.Context, BusyBatch) error
	// Reject ends an input that cannot be started.
	Reject(ctx context.Context, inputID string) error
	// Drainable lists the sessions that have platform messages to start.
	Drainable(context.Context) ([]BusySession, error)
}

// replySurfaces gives the surface for the replies of a bot on a channel
// outside the handling of an inbound message. It is the surface an inbound
// message's replies go through, so what waited in the queue is answered with
// the channel's own previews, buttons and capabilities.
type replySurfaces interface {
	Surface(ctx context.Context, botID string, channelType channel.ChannelType) (channel.StreamReplySender, error)
}

// SetBusyQueue configures the queue busy sessions park input in, and where the
// replies of the platform messages started from it are delivered.
func (p *ChannelInboundProcessor) SetBusyQueue(queue BusyQueue, surfaces replySurfaces) {
	if p == nil {
		return
	}
	p.busyQueue = queue
	p.replySurfaces = surfaces
}

// queuedReaction is the mark a platform message wears while it waits.
const queuedReaction = "⏳"

// queuedHintSupported reports whether the channel can show that a message is
// waiting: it takes reactions, and takes one back by the message it sits on.
// A reaction that cannot be removed would stay on the message after the reply,
// so a channel that needs the reaction's own id to remove it is not marked.
func queuedHintSupported(caps channel.ChannelCapabilities) bool {
	return caps.Reactions && caps.ReactionRemoveKey == channel.ReactionRemoveByMessageID
}

// busyTurn is a turn that met a busy session, with what the queue needs to
// store it and answer it later.
type busyTurn struct {
	cmd      turn.StartTurnCommand
	claims   auth.RouteCredentials
	msg      channel.InboundMessage
	identity InboundIdentity
	sender   channel.StreamReplySender
	caps     channel.ChannelCapabilities
	// fail shows the sender why the message could not wait.
	fail func(error)
}

// parkBusyTurn puts a turn that met a busy session in the session's queue. The
// sender is told only when the queue is full; otherwise the message waits, with
// a mark on it where the channel can show one, and its reply comes when its run
// has been through.
func (p *ChannelInboundProcessor) parkBusyTurn(ctx context.Context, in busyTurn) error {
	cmd := in.cmd
	origin := BusyOriginPlatform
	if isLocalChannelType(in.msg.Channel) {
		origin = BusyOriginFollowUp
	}
	payload, err := turnwire.NewQueuedInput(cmd, &in.claims).Marshal()
	if err != nil {
		return err
	}
	invocationID := strings.TrimSpace(cmd.IdempotencyKey)
	if invocationID == "" {
		invocationID = "turn:" + uuid.NewString()
	}
	err = p.busyQueue.Enqueue(ctx, BusyInput{
		BotID: cmd.BotID, SessionID: cmd.ThreadID, Origin: origin,
		InvocationID: invocationID, MergeKey: busyMergeKey(cmd), Payload: payload, ReceivedAt: in.msg.ReceivedAt,
	})
	switch {
	case errors.Is(err, ErrBusyQueueFull):
		in.fail(apperror.New(apperror.CodeSessionQueueFull, nil))
		return nil
	case errors.Is(err, ErrBusyQueueConflict):
		// The same message delivered again, which the queue already holds.
		return nil
	case err != nil:
		return err
	}
	if p.logger != nil {
		p.logger.InfoContext(ctx, "inbound turn queued: thread busy",
			slog.String("channel", in.msg.Channel.String()),
			slog.String("external_message_id", cmd.ExternalMessageID),
		)
	}
	if origin == BusyOriginPlatform {
		p.markQueued(ctx, in)
		p.kickBusyQueue(ctx, cmd.BotID, cmd.ThreadID)
		return nil
	}
	// The server starts a local channel's input; it learns of it the way it
	// learns of discuss input.
	p.wakePending(ctx, cmd.TeamID, cmd.BotID, cmd.ThreadID)
	return nil
}

// busyMergeKey groups the messages one run may answer together: those of one
// sender, next to each other in the queue. A message that names a command or a
// skill stands alone.
func busyMergeKey(cmd turn.StartTurnCommand) string {
	if cmd.AgentCommand != "" || cmd.SkillActivation != nil || len(cmd.RequestedSkills) > 0 {
		return ""
	}
	return firstNonEmpty(cmd.SourceChannelIdentityID, cmd.UserID)
}

// markQueued shows on the message that it is waiting, in a channel that can.
func (p *ChannelInboundProcessor) markQueued(ctx context.Context, in busyTurn) {
	if !queuedHintSupported(in.caps) || in.cmd.ReplyTarget == "" || in.cmd.ExternalMessageID == "" {
		return
	}
	err := in.sender.React(ctx, channel.ReactRequest{
		Target: in.cmd.ReplyTarget, MessageID: in.cmd.ExternalMessageID, Emoji: queuedReaction,
	})
	if err != nil && p.logger != nil {
		p.logger.WarnContext(ctx, "mark queued message failed",
			slog.String("channel", in.msg.Channel.String()), slog.Any("error", err))
	}
}

// unmarkQueued takes the mark off a message whose input has left the queue.
func (p *ChannelInboundProcessor) unmarkQueued(ctx context.Context, surface channel.StreamReplySender, channelType channel.ChannelType, caps channel.ChannelCapabilities, cmd turn.StartTurnCommand) {
	if surface == nil || !queuedHintSupported(caps) || cmd.ReplyTarget == "" || cmd.ExternalMessageID == "" {
		return
	}
	err := surface.React(ctx, channel.ReactRequest{
		Target: cmd.ReplyTarget, MessageID: cmd.ExternalMessageID, Emoji: queuedReaction, Remove: true,
	})
	if err != nil && p.logger != nil {
		p.logger.WarnContext(ctx, "unmark queued message failed",
			slog.String("channel", channelType.String()), slog.Any("error", err))
	}
}

// wakePending tells the server that a session has queued input, over the same
// port it is told about discuss input.
func (p *ChannelInboundProcessor) wakePending(ctx context.Context, teamID, botID, sessionID string) {
	switch {
	case p.discussWaker != nil:
		p.discussWaker.Wake(ctx, teamID, botID, sessionID)
	case p.logger != nil:
		p.logger.WarnContext(ctx, "waker not configured; the sweep starts this input",
			slog.String("bot_id", botID), slog.String("session_id", sessionID))
	}
}

// drainCoalescer runs one drain per session at a time. A call that arrives
// while one runs makes it go round once more, so a trigger is never lost to the
// moment the drain decides it is done.
type drainCoalescer struct {
	mu     sync.Mutex
	active map[string]*drainRound
}

type drainRound struct{ again bool }

func (c *drainCoalescer) run(key string, drain func()) {
	c.mu.Lock()
	if round, running := c.active[key]; running {
		round.again = true
		c.mu.Unlock()
		return
	}
	if c.active == nil {
		c.active = make(map[string]*drainRound)
	}
	round := &drainRound{}
	c.active[key] = round
	c.mu.Unlock()
	for {
		drain()
		c.mu.Lock()
		if !round.again {
			delete(c.active, key)
			c.mu.Unlock()
			return
		}
		round.again = false
		c.mu.Unlock()
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
