package inbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/channel/delivery"
)

// kickBusyQueue starts, off the caller's goroutine, whatever platform messages
// wait for the session. It is called when a message is parked, and when a run
// this process was consuming has ended: those are the moments a session is
// likely to be free. The sweep covers the rest.
func (p *ChannelInboundProcessor) kickBusyQueue(ctx context.Context, botID, sessionID string) {
	if p == nil || p.busyQueue == nil || p.replySurfaces == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go p.busyDrains.run(botID+":"+sessionID, func() { p.drainBusyQueue(ctx, botID, sessionID) })
}

// drainBusyQueue starts the session's waiting batches one after another, and
// delivers the reply of each before it takes the next. It stops at the first
// batch that cannot start now.
func (p *ChannelInboundProcessor) drainBusyQueue(ctx context.Context, botID, sessionID string) {
	for ctx.Err() == nil {
		batch, ok, err := p.busyQueue.ClaimBatch(ctx, botID, sessionID)
		if err != nil {
			p.logBusyQueue(ctx, "claim queued batch failed", sessionID, err)
			return
		}
		if !ok || !p.startBusyBatch(ctx, batch) {
			return
		}
	}
}

// startBusyBatch starts the run that answers a claimed batch and delivers its
// reply. It reports whether the next batch is worth trying.
func (p *ChannelInboundProcessor) startBusyBatch(ctx context.Context, batch BusyBatch) bool {
	inputs := p.readBusyBatch(ctx, batch)
	if len(inputs) == 0 {
		return true
	}
	merged, err := turnwire.MergeQueued(inputs)
	if err != nil {
		p.rejectBusyBatch(ctx, batch, err)
		return true
	}
	cmd := merged.Command
	cmd.NoDefer = true
	cmd.IdempotencyKey = "queued:" + batch.ID
	channelType := channel.ChannelType(cmd.CurrentChannel)
	var desc channel.Descriptor
	if p.registry != nil {
		desc, _ = p.registry.GetDescriptor(channelType) //nolint:errcheck // descriptor lookup is best-effort
	}
	// The reply goes through the surface an inbound message's reply goes
	// through. A batch whose channel has none now, such as a connection that is
	// down, is not started: it waits for the next pass instead of running with
	// nowhere to answer.
	surface, err := p.replySurfaces.Surface(ctx, cmd.BotID, channelType)
	if err != nil {
		p.logBusyQueue(ctx, "open reply surface", batch.SessionID, err)
		p.releaseBusyBatch(ctx, batch)
		return false
	}
	if merged.Claims != nil {
		cmd.ChatToken, cmd.Token = p.signQueuedTurn(ctx, *merged.Claims)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	streamKey := strings.TrimSpace(cmd.BotID) + ":" + strings.TrimSpace(cmd.RouteID)
	p.activeStreams.Store(streamKey, cancel)
	defer p.activeStreams.Delete(streamKey)

	handle, err := p.turnSvc.StartTurn(streamCtx, cmd)
	switch {
	case err == nil:
	case errors.Is(err, turn.ErrDuplicateTurn):
		// The run exists already: an earlier attempt admitted it and did not get
		// to settle the batch.
		p.settleBusyBatch(ctx, batch)
		p.unmarkBusyBatch(ctx, surface, channelType, desc.Capabilities, inputs)
		return false
	case errors.Is(err, turn.ErrSessionBusy):
		p.releaseBusyBatch(ctx, batch)
		return false
	case errors.Is(err, turn.ErrTeamNotServed):
		p.rejectBusyBatch(ctx, batch, err)
		return true
	default:
		p.logBusyQueue(ctx, "start queued batch failed", batch.SessionID, err)
		p.releaseBusyBatch(ctx, batch)
		return false
	}
	p.settleBusyBatch(ctx, batch)
	p.unmarkBusyBatch(ctx, surface, channelType, desc.Capabilities, inputs)

	p.deliverBusyRun(ctx, streamCtx, cmd, merged, desc, surface, handle)
	return true
}

// readBusyBatch restores the commands of a batch. An input that cannot be read,
// or that is not for this session, is rejected so it does not hold up the ones
// behind it.
func (p *ChannelInboundProcessor) readBusyBatch(ctx context.Context, batch BusyBatch) []turnwire.QueuedInput {
	var inputs []turnwire.QueuedInput
	for _, item := range batch.Inputs {
		input := turnwire.ParseQueuedInput(item.Payload)
		cmd, err := input.StartCommand()
		switch {
		case err != nil:
		case strings.TrimSpace(cmd.BotID) != batch.BotID || strings.TrimSpace(cmd.ThreadID) != batch.SessionID:
			err = errors.New("queued command belongs to another session")
		case strings.TrimSpace(cmd.TeamID) == "":
			err = errors.New("queued command has no team")
		case strings.TrimSpace(cmd.Query) == "" && len(cmd.Attachments) == 0:
			err = errors.New("queued command has no query or attachments")
		}
		if err != nil {
			p.logBusyQueue(ctx, "queued input cannot be started; rejecting it", batch.SessionID, err)
			if rejectErr := p.busyQueue.Reject(ctx, item.ID); rejectErr != nil {
				p.logBusyQueue(ctx, "reject queued input failed", batch.SessionID, rejectErr)
			}
			continue
		}
		input.ItemID = item.ID
		inputs = append(inputs, input)
	}
	return inputs
}

// signQueuedTurn signs the credentials of a queued turn from the identities
// stored with it, with the function a new inbound message's turn is signed
// with. Without a secret none are issued.
func (p *ChannelInboundProcessor) signQueuedTurn(ctx context.Context, claims auth.RouteCredentials) (chatToken, sessionBearer string) {
	chatToken, sessionBearer, err := auth.IssueRouteCredentials(claims, p.jwtSecret, p.tokenTTL)
	if err != nil {
		p.logBusyQueue(ctx, "issue route credentials failed", "", err)
		return "", ""
	}
	return chatToken, sessionBearer
}

// deliverBusyRun sends the reply of a started batch. The reply quotes the last
// message of the batch, and the reply window is judged from when that message
// was received, not from now.
func (p *ChannelInboundProcessor) deliverBusyRun(ctx, streamCtx context.Context, cmd turn.StartTurnCommand, merged turnwire.QueuedTurn, desc channel.Descriptor, surface channel.StreamReplySender, handle turn.RunHandle) {
	channelType := channel.ChannelType(cmd.CurrentChannel)
	target := strings.TrimSpace(cmd.ReplyTarget)
	sourceMessageID := strings.TrimSpace(cmd.ExternalMessageID)
	replyRef := &channel.ReplyRef{Target: target}
	if sourceMessageID != "" {
		replyRef.MessageID = sourceMessageID
	}
	var observe func(context.Context, channel.StreamEvent)
	if p.observer != nil && !isLocalChannelType(channelType) {
		observe = func(ctx context.Context, event channel.StreamEvent) {
			p.observer.OnStreamEvent(ctx, strings.TrimSpace(cmd.BotID), channelType, event)
		}
	}
	out, err := delivery.Open(ctx, delivery.Config{
		Surface:         surface,
		Target:          target,
		Reply:           replyRef,
		SourceMessageID: sourceMessageID,
		Metadata: map[string]any{
			"route_id":          strings.TrimSpace(cmd.RouteID),
			"conversation_type": cmd.ConversationType,
		},
		ReceivedAt:    merged.ReceivedAt,
		HideToolCalls: !isLocalChannelType(channelType) && !p.shouldShowToolCallsInIM(ctx, cmd.BotID),
		Observe:       observe,
		Logger:        p.logger,
		LogAttrs: []slog.Attr{
			slog.String("channel", channelType.String()),
			slog.String("channel_identity_id", cmd.SourceChannelIdentityID),
			slog.String("user_id", cmd.UserID),
		},
	})
	if err != nil {
		p.logBusyQueue(ctx, "open queued reply failed", cmd.ThreadID, err)
		handle.Cancel()
		return
	}
	defer func() {
		if closeErr := out.Close(ctx); closeErr != nil {
			p.logBusyQueue(ctx, "queued reply close failed", cmd.ThreadID, closeErr)
		}
	}()
	runErr := p.consumeRun(ctx, runReply{
		out: out, handle: handle, botID: cmd.BotID, channelType: channelType, caps: desc.Capabilities,
		target: target, sourceMessageID: sourceMessageID,
		sessionID: cmd.ThreadID, since: merged.ReceivedAt.Add(-time.Minute),
		localizer: p.localizer(ctx, cmd.BotID), streamCtx: streamCtx,
	})
	if runErr != nil && !stoppedByCommand(ctx, streamCtx) {
		p.logBusyQueue(ctx, "queued reply failed", cmd.ThreadID, runErr)
	}
}

// unmarkBusyBatch takes the waiting mark off the messages of a batch that has
// left the queue.
func (p *ChannelInboundProcessor) unmarkBusyBatch(ctx context.Context, surface channel.StreamReplySender, channelType channel.ChannelType, caps channel.ChannelCapabilities, inputs []turnwire.QueuedInput) {
	for _, input := range inputs {
		if cmd, err := input.StartCommand(); err == nil {
			p.unmarkQueued(ctx, surface, channelType, caps, cmd)
		}
	}
}

func (p *ChannelInboundProcessor) settleBusyBatch(ctx context.Context, batch BusyBatch) {
	if err := p.busyQueue.SettleBatch(ctx, batch); err != nil {
		// The run exists. Until the batch is settled its lease runs out and the
		// next claim finds the run under the same invocation id.
		p.logBusyQueue(ctx, "settle queued batch failed", batch.SessionID, err)
	}
}

func (p *ChannelInboundProcessor) releaseBusyBatch(ctx context.Context, batch BusyBatch) {
	if err := p.busyQueue.ReleaseBatch(ctx, batch); err != nil {
		p.logBusyQueue(ctx, "release queued batch failed", batch.SessionID, err)
	}
}

func (p *ChannelInboundProcessor) rejectBusyBatch(ctx context.Context, batch BusyBatch, cause error) {
	p.logBusyQueue(ctx, "queued batch cannot be started; rejecting it", batch.SessionID, cause)
	for _, item := range batch.Inputs {
		if err := p.busyQueue.Reject(ctx, item.ID); err != nil {
			p.logBusyQueue(ctx, "reject queued input failed", batch.SessionID, err)
		}
	}
}

func (p *ChannelInboundProcessor) logBusyQueue(ctx context.Context, what, sessionID string, err error) {
	if p.logger == nil {
		return
	}
	p.logger.WarnContext(ctx, "busy queue operation failed",
		slog.String("what", what), slog.String("session_id", sessionID), slog.Any("error", err))
}

// busyQueueSweepInterval is how often the sweep looks for platform messages
// that nothing has started. A message is normally started when the run it
// waited for ends in this process; the sweep covers a run that ended in
// another process, a message parked while the run ended, a claim whose lease
// ran out, and a restart.
const busyQueueSweepInterval = time.Minute

// BusyQueueSweep is the backstop for the prompt triggers of the busy queue.
type BusyQueueSweep struct {
	processor *ChannelInboundProcessor
	stop      context.CancelFunc
	done      chan struct{}
}

func NewBusyQueueSweep(processor *ChannelInboundProcessor) *BusyQueueSweep {
	return &BusyQueueSweep{processor: processor}
}

// Start sweeps at once and then on every interval, until Stop.
func (s *BusyQueueSweep) Start(ctx context.Context) error {
	if s.processor == nil || s.processor.busyQueue == nil || s.processor.replySurfaces == nil {
		return errors.New("busy queue sweep is not configured")
	}
	workerCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = stop
	s.done = make(chan struct{})
	ticker := time.NewTicker(busyQueueSweepInterval)
	go func() {
		defer close(s.done)
		defer ticker.Stop()
		s.run(workerCtx, ticker.C)
	}()
	return nil
}

// Stop ends the sweep. Batches already started are not touched.
func (s *BusyQueueSweep) Stop() {
	if s.stop != nil {
		s.stop()
	}
	if s.done != nil {
		<-s.done
	}
}

func (s *BusyQueueSweep) run(ctx context.Context, ticks <-chan time.Time) {
	for {
		s.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (s *BusyQueueSweep) sweep(ctx context.Context) {
	p := s.processor
	sessions, err := p.busyQueue.Drainable(ctx)
	if err != nil {
		p.logBusyQueue(ctx, "list drainable sessions failed", "", fmt.Errorf("busy queue sweep: %w", err))
		return
	}
	for _, session := range sessions {
		p.kickBusyQueue(ctx, session.BotID, session.SessionID)
	}
}
