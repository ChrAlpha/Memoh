package application

import (
	"context"
	"errors"
	"log/slog"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

const (
	// queueSweepInterval is how often the sweep looks for queued input that
	// nothing has started. The prompt triggers are a run ending and an input
	// being queued; the sweep is for what they miss: a process that died, an
	// input queued behind a run of another process, a claim whose lease ran out.
	queueSweepInterval = time.Minute
	// queueSettledRetention is how long a consumed or cancelled input stays in
	// the table. It is what recognizes a redelivered message as one already
	// taken, so it outlasts the platforms' redelivery.
	queueSettledRetention = 24 * time.Hour
)

// eachScope runs fn in every database scope the resume scope provider names,
// page after page.
func (s *Service) eachScope(ctx context.Context, what string, fn func(scopeCtx context.Context, scope string)) {
	scopes := s.sessionResumeScopes()
	cursor := ""
	for ctx.Err() == nil {
		page, err := scopes.ListScopePage(ctx, cursor, 16)
		if err == nil {
			err = validateResumeScopePage(page, cursor)
		}
		if err != nil {
			s.logger.WarnContext(ctx, "list sweep scopes failed", slog.String("sweep", what), slog.Any("error", err))
			return
		}
		for _, scope := range page.Scopes {
			scopeCtx, err := bindSessionResumeScope(ctx, scopes, scope)
			if err != nil {
				s.logger.WarnContext(ctx, "bind sweep scope failed", slog.String("sweep", what), slog.Any("error", err))
				continue
			}
			fn(scopeCtx, scope)
		}
		if page.Complete {
			return
		}
		cursor = page.NextCursor
	}
}

// QueueSweep is the backstop for the prompt triggers of the follow-up queue. At
// start and then on every interval it takes in what servers of the previous
// release left in Redis, settles promotions that lost their process, starts
// the batches that wait for a free session, and deletes inputs settled long
// ago.
//
// Several servers may sweep at once. Which of them starts a batch is decided by
// the queue's claim and the batch's invocation id.
type QueueSweep struct {
	service *Service
	stop    context.CancelFunc
	done    chan struct{}
}

func NewQueueSweep(service *Service) *QueueSweep {
	return &QueueSweep{service: service}
}

// Start runs the first sweep immediately and the following ones on the
// interval, until Stop. The sweep outlives ctx: the runs it starts are stopped
// by the runtime manager on shutdown, not by this worker.
func (q *QueueSweep) Start(ctx context.Context) error {
	if q.service == nil || q.service.queue == nil || q.service.sessionManager == nil {
		return errors.New("queue sweep is not configured")
	}
	workerCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	q.stop = stop
	q.done = make(chan struct{})
	ticker := time.NewTicker(queueSweepInterval)
	go func() {
		defer close(q.done)
		defer ticker.Stop()
		q.run(workerCtx, ticker.C, time.Now)
	}()
	return nil
}

// Stop ends the sweep. Runs already started are not touched.
func (q *QueueSweep) Stop() {
	if q.stop != nil {
		q.stop()
	}
	if q.done != nil {
		<-q.done
	}
}

// run sweeps once, then once per tick, until ctx ends. The clock and the ticks
// are the caller's so a test can drive both.
func (q *QueueSweep) run(ctx context.Context, ticks <-chan time.Time, now func() time.Time) {
	for {
		q.sweep(ctx, now())
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (q *QueueSweep) sweep(ctx context.Context, now time.Time) {
	q.service.eachScope(ctx, "queue sweep", func(scopeCtx context.Context, scope string) {
		q.service.sweepQueueScope(scopeCtx, scope, now)
	})
}

func (s *Service) sweepQueueScope(ctx context.Context, teamID string, now time.Time) {
	s.importLegacyFollowUps(ctx, teamID)
	s.reconcileStalePromotions(ctx, now)
	keys, err := s.queue.Drainable(ctx, sessionruntime.QueueOriginFollowUp)
	if err != nil {
		s.logger.WarnContext(ctx, "list drainable sessions failed", slog.Any("error", err))
	}
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.admitPending(context.WithoutCancel(ctx), key.BotID, key.SessionID); err != nil && !errors.Is(err, sessionruntime.ErrManagerClosed) {
			s.logger.WarnContext(ctx, "queue sweep admission failed",
				slog.String("session_id", key.SessionID), slog.Any("error", err))
		}
	}
	if _, err := s.queue.PurgeSettled(ctx, now.Add(-queueSettledRetention)); err != nil {
		s.logger.WarnContext(ctx, "purge settled queue inputs failed", slog.Any("error", err))
	}
}

// importLegacyFollowUps takes in the follow-ups a server of the previous
// release wrote to Redis. See sessionruntime.ImportLegacyFollowUps.
func (s *Service) importLegacyFollowUps(ctx context.Context, teamID string) {
	moved, err := s.sessionManager.ImportLegacyFollowUps(ctx, s.queue, legacyFollowUpEntry(teamID))
	if err != nil {
		s.logger.WarnContext(ctx, "import legacy follow-ups failed", slog.Any("error", err))
	}
	if moved > 0 {
		s.logger.InfoContext(ctx, "imported legacy follow-ups into the queue table", slog.Int("count", moved))
	}
}

// legacyFollowUpEntry writes a follow-up of the previous release the way the
// queue holds it: without the credentials the old release stored with it, and
// under the invocation id it had. An input of a team other than the one the
// pass is bound to is left for the pass of its own scope.
func legacyFollowUpEntry(teamID string) sessionruntime.LegacyFollowUpConverter {
	return func(item sessionruntime.LegacyFollowUp) (sessionruntime.QueueEnqueue, bool, error) {
		input := turnwire.ParseQueuedInput(item.Payload)
		cmd, err := input.StartCommand()
		if err != nil {
			return sessionruntime.QueueEnqueue{}, false, err
		}
		if teamID != "" && cmd.TeamID != teamID {
			return sessionruntime.QueueEnqueue{}, false, nil
		}
		input.Command.Token = ""
		input.Command.ChatToken = ""
		payload, err := input.Marshal()
		if err != nil {
			return sessionruntime.QueueEnqueue{}, false, err
		}
		return sessionruntime.QueueEnqueue{
			Key: item.Key, Origin: sessionruntime.QueueOriginFollowUp, InvocationID: item.InvocationID,
			Payload: payload, EnqueuedDuringRunID: item.EnqueuedDuringRunID, CreatedAt: item.CreatedAt,
		}, true, nil
	}
}
