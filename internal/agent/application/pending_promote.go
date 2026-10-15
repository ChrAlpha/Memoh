package application

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

// promotionGrace is how long a promotion may be without an outcome before the
// sweep settles it. The three steps of a promotion take milliseconds, so what
// is older than this lost its process.
const promotionGrace = 2 * time.Minute

// PromoteFollowUpToSteer hands a queued follow-up to the run in flight as a
// steer. The steer stays in the live backend, which keeps it only as long as
// the run; the follow-up stays in the queue's table until the run has taken it.
//
// The order is what makes a crash harmless. First the queue marks the input as
// being promoted, which removes it from what a drainer may start. Then the steer
// is written under the input's own id, a write that finds its own earlier
// result when repeated. Only then does the queue record the promotion. A crash
// after the first step leaves an input with no steer, which the sweep puts
// back; after the second, an input with a steer, which the sweep or the run
// completes. The input is never in the queue and in the steer queue at once.
func (s *Service) PromoteFollowUpToSteer(ctx context.Context, botID, sessionID string, ref sessionruntime.FollowUpPendingRef) (sessionruntime.PromoteFollowUpResult, error) {
	queue, err := s.queueStore()
	if err != nil {
		return sessionruntime.PromoteFollowUpResult{}, err
	}
	runtime, err := s.liveQueueRuntime()
	if err != nil {
		return sessionruntime.PromoteFollowUpResult{}, err
	}
	key := sessionruntime.Key{BotID: botID, SessionID: sessionID}
	promotion, err := queue.BeginPromotion(ctx, key, string(ref.ItemID))
	if err != nil {
		return sessionruntime.PromoteFollowUpResult{}, err
	}
	if promotion.Replay {
		steer, ok, err := runtime.LookupSteer(ctx, key, sessionruntime.SteerItemID(promotion.Item.ID))
		if err != nil {
			return sessionruntime.PromoteFollowUpResult{}, err
		}
		if !ok {
			return sessionruntime.PromoteFollowUpResult{}, sessionruntime.ErrQueueNotPending
		}
		return sessionruntime.PromoteFollowUpResult{FollowUp: ref, Steer: steer}, nil
	}

	payload, err := turnwire.QueuedInput{Text: turnwire.ParseQueuedInput(promotion.Item.Payload).VisibleText()}.Marshal()
	if err == nil {
		var steer sessionruntime.SteerItem
		steer, err = runtime.EnqueueSteer(ctx, key, promotion.Item.ID, "promote:"+promotion.Item.ID, payload)
		if err == nil {
			if completeErr := queue.CompletePromotion(ctx, promotion.Item.ID, promotion.Token); completeErr != nil && s.logger != nil {
				// The steer is live. The sweep records the promotion.
				s.logger.WarnContext(ctx, "complete follow-up promotion failed",
					slog.String("item_id", promotion.Item.ID), slog.Any("error", completeErr))
			}
			return sessionruntime.PromoteFollowUpResult{FollowUp: ref, Steer: steer}, nil
		}
	}
	if abortErr := queue.AbortPromotion(ctx, promotion.Item.ID, promotion.Token); abortErr != nil && s.logger != nil {
		// The input stays promoting without a steer until the sweep puts it
		// back.
		s.logger.WarnContext(ctx, "abort follow-up promotion failed",
			slog.String("item_id", promotion.Item.ID), slog.Any("error", abortErr))
	}
	return sessionruntime.PromoteFollowUpResult{}, err
}

// consumePromotedInput records, once a run has applied a steer, that the input
// it was promoted from is taken. A steer that was not promoted from a queued
// input matches nothing. A failure leaves the record to the sweep, which reads
// the steer.
func (s *Service) consumePromotedInput(ctx context.Context, steerID string) {
	if s == nil || s.queue == nil {
		return
	}
	if err := s.queue.ConsumePromoted(context.WithoutCancel(ctx), steerID); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "record applied promoted input failed",
			slog.String("item_id", steerID), slog.Any("error", err))
	}
}

// reconcileSessionPromotions settles the promotions of a session whose run has
// just ended. Its steers are closed by then, so each promoted input is either
// applied or goes back to the queue.
func (s *Service) reconcileSessionPromotions(ctx context.Context, botID, sessionID string) {
	if s.queue == nil || s.sessionManager == nil {
		return
	}
	inputs, err := s.queue.Promotions(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID})
	if err != nil {
		s.logReconcile(ctx, err)
		return
	}
	for _, input := range inputs {
		s.reconcilePromotion(ctx, input)
	}
}

// reconcileStalePromotions settles the promotions that have had no outcome for
// longer than promotionGrace.
func (s *Service) reconcileStalePromotions(ctx context.Context, now time.Time) {
	if s.queue == nil || s.sessionManager == nil {
		return
	}
	inputs, err := s.queue.StalePromotions(ctx, now.Add(-promotionGrace))
	if err != nil {
		s.logReconcile(ctx, err)
		return
	}
	for _, input := range inputs {
		s.reconcilePromotion(ctx, input)
	}
}

// reconcilePromotion reads the steer a promoted input stands for and settles
// the input by it: applied is consumed; still waiting for the run completes the
// promotion if a crash left it half done; refused, closed or gone is put back
// in the queue, because its text was never delivered.
//
// An applied steer whose record the live backend no longer holds reads as gone
// and runs once more. That needs the record to be lost, by its retention or by
// the backend, between the application and the first pass that settles it; the
// input is then delivered twice rather than never.
func (s *Service) reconcilePromotion(ctx context.Context, input sessionruntime.QueuedInput) {
	key := sessionruntime.Key{BotID: input.BotID, SessionID: input.SessionID}
	steer, ok, err := s.sessionManager.LookupSteer(ctx, key, sessionruntime.SteerItemID(input.ID))
	if err != nil {
		if !errors.Is(err, sessionruntime.ErrLiveQueueUnavailable) {
			s.logReconcile(ctx, err)
		}
		return
	}
	switch {
	case ok && steer.Status == sessionruntime.QueueApplied:
		err = s.queue.ConsumePromoted(ctx, input.ID)
	case ok && (steer.Status == sessionruntime.QueueAccepted || steer.Status == sessionruntime.QueueClaimed):
		if input.State == sessionruntime.QueuedStatePromoting && strings.TrimSpace(input.ClaimToken) != "" {
			err = s.queue.CompletePromotion(ctx, input.ID, input.ClaimToken)
		}
	default:
		err = s.queue.RequeuePromoted(ctx, input.ID)
	}
	if err != nil {
		s.logReconcile(ctx, err)
	}
}

func (s *Service) logReconcile(ctx context.Context, err error) {
	if s.logger != nil {
		s.logger.WarnContext(ctx, "reconcile follow-up promotions failed", slog.Any("error", err))
	}
}
