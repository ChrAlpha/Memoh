package pending

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
)

// PromotionGrace is how long a promotion may be without an outcome before the
// sweep settles it. The three steps of a promotion take milliseconds, so what
// is older than this lost its process.
const PromotionGrace = 2 * time.Minute

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
func (in *Inputs) PromoteFollowUpToSteer(ctx context.Context, botID, sessionID string, ref sessionruntime.FollowUpPendingRef) (sessionruntime.PromoteFollowUpResult, error) {
	queue, err := in.queueStore()
	if err != nil {
		return sessionruntime.PromoteFollowUpResult{}, err
	}
	key := sessionruntime.Key{BotID: botID, SessionID: sessionID}
	promotion, err := queue.BeginPromotion(ctx, key, string(ref.ItemID))
	if err != nil {
		return sessionruntime.PromoteFollowUpResult{}, err
	}
	if promotion.Replay {
		steer, ok, err := in.runtime.LookupSteer(ctx, key, sessionruntime.SteerItemID(promotion.Item.ID))
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
		steer, err = in.runtime.EnqueueSteer(ctx, key, promotion.Item.ID, "promote:"+promotion.Item.ID, payload)
		if err == nil {
			if completeErr := queue.CompletePromotion(ctx, promotion.Item.ID, promotion.Token); completeErr != nil {
				// The steer is live. The sweep records the promotion.
				in.logger.WarnContext(ctx, "complete follow-up promotion failed",
					slog.String("item_id", promotion.Item.ID), slog.Any("error", completeErr))
			}
			return sessionruntime.PromoteFollowUpResult{FollowUp: ref, Steer: steer}, nil
		}
	}
	if abortErr := queue.AbortPromotion(ctx, promotion.Item.ID, promotion.Token); abortErr != nil {
		// The input stays promoting without a steer until the sweep puts it
		// back.
		in.logger.WarnContext(ctx, "abort follow-up promotion failed",
			slog.String("item_id", promotion.Item.ID), slog.Any("error", abortErr))
	}
	return sessionruntime.PromoteFollowUpResult{}, err
}

// reconcileSessionPromotions settles the promotions of a session whose run has
// just ended. Its steers are closed by then, so each promoted input is either
// applied or goes back to the queue.
func (in *Inputs) reconcileSessionPromotions(ctx context.Context, botID, sessionID string) {
	inputs, err := in.queue.Promotions(ctx, sessionruntime.Key{BotID: botID, SessionID: sessionID})
	if err != nil {
		in.logReconcile(ctx, err)
		return
	}
	for _, input := range inputs {
		in.reconcilePromotion(ctx, input)
	}
}

// ReconcileStalePromotions settles the promotions that have had no outcome for
// longer than PromotionGrace.
func (in *Inputs) ReconcileStalePromotions(ctx context.Context, now time.Time) {
	inputs, err := in.queue.StalePromotions(ctx, now.Add(-PromotionGrace))
	if err != nil {
		in.logReconcile(ctx, err)
		return
	}
	for _, input := range inputs {
		in.reconcilePromotion(ctx, input)
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
func (in *Inputs) reconcilePromotion(ctx context.Context, input sessionruntime.QueuedInput) {
	key := sessionruntime.Key{BotID: input.BotID, SessionID: input.SessionID}
	steer, ok, err := in.runtime.LookupSteer(ctx, key, sessionruntime.SteerItemID(input.ID))
	if err != nil {
		if !errors.Is(err, sessionruntime.ErrLiveQueueUnavailable) {
			in.logReconcile(ctx, err)
		}
		return
	}
	switch {
	case ok && steer.Status == sessionruntime.QueueApplied:
		err = in.queue.ConsumePromoted(ctx, input.ID)
	case ok && (steer.Status == sessionruntime.QueueAccepted || steer.Status == sessionruntime.QueueClaimed):
		if input.State == sessionruntime.QueuedStatePromoting && strings.TrimSpace(input.ClaimToken) != "" {
			err = in.queue.CompletePromotion(ctx, input.ID, input.ClaimToken)
		}
	default:
		err = in.queue.RequeuePromoted(ctx, input.ID)
	}
	if err != nil {
		in.logReconcile(ctx, err)
	}
}

func (in *Inputs) logReconcile(ctx context.Context, err error) {
	in.logger.WarnContext(ctx, "reconcile follow-up promotions failed", slog.Any("error", err))
}
