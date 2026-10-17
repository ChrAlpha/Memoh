package application

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/telemetry"
)

// sessionLocks serializes work per session. An entry exists only while
// someone holds or waits for it, so the table does not grow with the number of
// sessions ever seen.
type sessionLocks struct {
	mu   sync.Mutex
	held map[string]*sessionLock
}

type sessionLock struct {
	mu    sync.Mutex
	users int
}

func (l *sessionLocks) lock(key string) (unlock func()) {
	l.mu.Lock()
	if l.held == nil {
		l.held = make(map[string]*sessionLock)
	}
	entry := l.held[key]
	if entry == nil {
		entry = &sessionLock{}
		l.held[key] = entry
	}
	entry.users++
	l.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.held, key)
		}
		l.mu.Unlock()
	}
}

// afterSessionTerminal is what the end of a session's run sets going, in the
// one order that is correct: the run's steers are closed first, so nothing
// started next meets a steer that still names the finished run; promoted input
// whose steer died unapplied goes back to the queue; then whatever waits for
// the session is admitted, queued input before an uncovered discuss batch.
func (s *Service) afterSessionTerminal(ctx context.Context, terminal sessionruntime.TerminalRun) {
	if s == nil || terminal.BotID == "" || terminal.SessionID == "" {
		return
	}
	s.closeSteerQueueForRun(ctx, terminal)
	s.reconcileSessionPromotions(ctx, terminal.BotID, terminal.SessionID)
	s.kickPending(ctx, terminal.BotID, terminal.SessionID)
}

// kickPending asks, off the caller's goroutine, what waits for the session. The
// call outlives the request that made it: its run does.
func (s *Service) kickPending(ctx context.Context, botID, sessionID string) {
	if s == nil || (s.queue == nil && s.queries == nil) {
		return
	}
	// The input that waited is its own trace, linked to the run or request that
	// let it start, rather than nested in a trace that has long ended.
	ctx = telemetry.ContextWithTrigger(context.WithoutCancel(ctx), telemetry.TriggerFrom(ctx))
	go func() {
		if _, err := s.admitPending(ctx, botID, sessionID); err != nil && !errors.Is(err, sessionruntime.ErrManagerClosed) && s.logger != nil {
			s.logger.WarnContext(ctx, "admit pending input failed",
				slog.String("session_id", sessionID), slog.Any("error", err))
		}
	}()
}

// admitPending is the one admission of what waits on a session: queued
// follow-ups first, then, when there are none to start, the external input a
// discuss session has not covered. It runs under a lock per session, so the
// terminal observer, an enqueue, the sweep and a wake from the channel process
// take turns instead of racing for the session's slot.
//
// It reports the discuss outcome; a queued batch that was started is a run
// that holds the session, which is DiscussBusy. That run's end asks again.
func (s *Service) admitPending(ctx context.Context, botID, sessionID string) (DiscussAdmission, error) {
	unlock := s.pendingSessions.lock(botID + ":" + sessionID)
	defer unlock()
	started, err := s.drainFollowUps(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID})
	if err != nil && s.logger != nil {
		// A queue that cannot be read must not stop discuss input from being
		// answered.
		s.logger.WarnContext(ctx, "drain follow-up queue failed",
			slog.String("session_id", sessionID), slog.Any("error", err))
	}
	if started {
		return DiscussBusy, nil
	}
	outcome, _, err := s.admitDiscuss(ctx, botID, sessionID)
	return outcome, err
}

// batchOutcome says what became of one claimed batch.
type batchOutcome int

const (
	// batchStarted means the batch's run exists.
	batchStarted batchOutcome = iota
	// batchRejected means no input of the batch could ever start, so the next
	// batch is worth a try.
	batchRejected
	// batchDeferred means the batch could not start now and waits.
	batchDeferred
)

// drainFollowUps starts the next batch of queued follow-ups when the session is
// free, and reports whether it started one.
func (s *Service) drainFollowUps(ctx context.Context, key sessionruntime.Key) (bool, error) {
	if s.queue == nil {
		return false, nil
	}
	for ctx.Err() == nil {
		batch, ok, err := s.queue.ClaimBatch(ctx, key, sessionruntime.QueueOriginFollowUp)
		if err != nil || !ok {
			return false, err
		}
		switch s.startQueuedBatch(ctx, batch) {
		case batchStarted:
			return true, nil
		case batchDeferred:
			return false, nil
		case batchRejected:
		}
	}
	return false, ctx.Err()
}

var (
	errQueuedCommandForeignSession = errors.New("queued command belongs to another session")
	errQueuedCommandWithoutTeam    = errors.New("queued command has no team")
	errQueuedCommandWithoutInput   = errors.New("queued command has no query or attachments")
)

// queuedInput restores one stored input. The command is the complete admission
// input the ingress recorded when it accepted the text, so starting it
// replays it rather than inferring the team or the sender from process state.
// An input missing any of that, or stored under a schema version this binary
// does not read, cannot be started.
func queuedInput(item sessionruntime.QueuedInput) (turnwire.QueuedInput, error) {
	input := turnwire.ParseQueuedInput(item.Payload)
	cmd, err := input.StartCommand()
	if err != nil {
		return turnwire.QueuedInput{}, err
	}
	switch {
	case strings.TrimSpace(cmd.BotID) != item.BotID || strings.TrimSpace(cmd.ThreadID) != item.SessionID:
		return turnwire.QueuedInput{}, errQueuedCommandForeignSession
	case strings.TrimSpace(cmd.TeamID) == "":
		return turnwire.QueuedInput{}, errQueuedCommandWithoutTeam
	case strings.TrimSpace(cmd.Query) == "" && len(cmd.Attachments) == 0:
		return turnwire.QueuedInput{}, errQueuedCommandWithoutInput
	}
	return input, nil
}

// startQueuedBatch admits the run that answers a claimed batch. The run's
// invocation id is the batch's, so a batch whose run already exists, as after
// a crash between admission and the settle, finds it and is settled instead of
// answered twice.
func (s *Service) startQueuedBatch(ctx context.Context, batch sessionruntime.QueueBatch) batchOutcome {
	var inputs []turnwire.QueuedInput
	for _, item := range batch.Items {
		input, err := queuedInput(item)
		if err != nil {
			s.rejectQueuedInput(ctx, item, err)
			continue
		}
		input.ItemID = item.ID
		inputs = append(inputs, input)
	}
	if len(inputs) == 0 {
		return batchRejected
	}
	merged, err := turnwire.MergeQueued(inputs)
	if err != nil {
		s.rejectQueuedBatch(ctx, batch, err)
		return batchRejected
	}
	cmd := merged.Command
	cmd.NoDefer = true
	cmd.IdempotencyKey = batch.InvocationID()
	s.signQueuedCommand(ctx, &cmd, merged.Claims)
	handle, err := s.StartTurn(ctx, cmd)
	switch {
	case err == nil:
		s.settleQueuedBatch(ctx, batch)
		// The session's subscribers see the run; the handle only needs a
		// consumer so its channels drain.
		go drainDeferredTurn(handle)
		return batchStarted
	case errors.Is(err, turn.ErrDuplicateTurn):
		s.settleQueuedBatch(ctx, batch)
		return batchDeferred
	case errors.Is(err, turn.ErrTeamNotServed):
		// The team is not one this instance serves, and that does not change
		// between attempts: retrying would only repeat it.
		s.rejectQueuedBatch(ctx, batch, err)
		return batchRejected
	case errors.Is(err, turn.ErrSessionBusy):
		s.releaseQueuedBatch(ctx, batch, nil)
	default:
		s.releaseQueuedBatch(ctx, batch, err)
	}
	return batchDeferred
}

// signQueuedCommand gives a dequeued turn credentials of its own, from the
// identities stored with it, through the function a new inbound message's turn
// is signed with. When that function issues none, as for a sender no account is
// bound to, the turn starts without, as the inbound turn would.
func (s *Service) signQueuedCommand(ctx context.Context, cmd *turn.StartTurnCommand, claims *auth.RouteCredentials) {
	if claims == nil {
		return
	}
	chatToken, token, err := s.signer.IssueRoute(*claims)
	if err != nil {
		if s.logger != nil {
			s.logger.WarnContext(ctx, "issue route credentials for a queued turn failed", slog.Any("error", err))
		}
		return
	}
	cmd.ChatToken, cmd.Token = chatToken, token
}

func (s *Service) settleQueuedBatch(ctx context.Context, batch sessionruntime.QueueBatch) {
	if err := s.queue.SettleBatch(ctx, batch); err != nil && s.logger != nil {
		// The run exists. Until the batch is settled its lease runs out and the
		// next claim finds the run under the same invocation id.
		s.logger.WarnContext(ctx, "settle queued batch failed",
			slog.String("batch_id", batch.ID), slog.Any("error", err))
	}
}

func (s *Service) releaseQueuedBatch(ctx context.Context, batch sessionruntime.QueueBatch, cause error) {
	if cause != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "start queued batch failed",
			slog.String("batch_id", batch.ID), slog.Any("error", cause))
	}
	if err := s.queue.ReleaseBatch(ctx, batch); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "release queued batch failed",
			slog.String("batch_id", batch.ID), slog.Any("error", err))
	}
}

func (s *Service) rejectQueuedBatch(ctx context.Context, batch sessionruntime.QueueBatch, cause error) {
	for _, item := range batch.Items {
		s.rejectQueuedInput(ctx, item, cause)
	}
}

// rejectQueuedInput ends an input this deployment can never start, so it does
// not hold up the ones behind it.
func (s *Service) rejectQueuedInput(ctx context.Context, item sessionruntime.QueuedInput, cause error) {
	if s.logger != nil {
		s.logger.WarnContext(ctx, "queued input cannot be started; rejecting it",
			slog.String("item_id", item.ID), slog.Any("error", cause))
	}
	if err := s.queue.Reject(ctx, item.ID, sessionruntime.QueueErrorFollowUpCommandInvalid); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "reject queued input failed",
			slog.String("item_id", item.ID), slog.Any("error", err))
	}
}

// drainDeferredTurn consumes a started run's channels. Server-owned runs need a
// consumer independent of the short admission, and the terminal observer
// schedules the next batch.
func drainDeferredTurn(handle turn.RunHandle) {
	if handle == nil {
		return
	}
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
