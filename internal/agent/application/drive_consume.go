package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/hooks"
)

// newRunObserver starts an observer for a run whose events have not arrived yet.
func newRunObserver() *runObserver {
	return &runObserver{}
}

// runObserver holds what one run's event stream said about how it ended. The
// consumer line writes it for every event; the driver reads it once the
// producer has stopped.
type runObserver struct {
	// idle is the watchdog whose expiry is a timeout rather than a stop.
	idle *idleCancel
	// declared is the first failure the run declared: an error event, a
	// refused publication, an executor cause, an idle timeout.
	declared error
	// hook says the declared failure came from the user-message hook, the one
	// setup failure a user has to be shown.
	hook bool
	// stoppedAtDeclare records whether the run was already stopped when the
	// first failure was declared. A failure declared first keeps its name; a
	// failure declared after a stop does not.
	stoppedAtDeclare bool
	// deferred says the terminal event parked the run on a decision.
	deferred bool
	// terminal is the terminal event, held back from publication until the
	// ending's history write has succeeded.
	terminal *native.StreamEvent
	// publishedFailure is the code of the failure event the run published, so
	// the same failure is not published twice.
	publishedFailure string
	// lost says a write or publication reported that another owner holds this
	// run. It is not a failure: that owner names the outcome.
	lost bool
	// phase is the stage the declared failure belongs to. A failure a
	// publication reported is named from its own stage rather than from the
	// stage the producer was in.
	phase runPhase
}

// observe records one event from the producer and returns the events to
// publish, in order: usually the event itself, nothing for a terminal event
// (the driver publishes that after the ending's history write), and nothing
// for a failure event whose code the run already published.
func (r *runObserver) observe(ctx context.Context, s *Service, botID string, assets *outboundAssets, stopped bool, ev native.StreamEvent) []native.StreamEvent {
	if ev.IsTerminal() {
		terminal := ev
		r.terminal = &terminal
		r.deferred = strings.TrimSpace(ev.ApprovalID) != ""
		if ev.Type == event.AgentEnd {
			// A clean end means an earlier retryable error recovered.
			r.declared = nil
			r.publishedFailure = ""
		}
		return nil
	}
	prepared := s.prepareRunEvent(ctx, botID, assets, ev)
	kept := prepared[:0]
	for _, out := range prepared {
		if cause := agentStreamFailure(out); cause != nil {
			r.declare(cause, stopped)
			code := string(publicFailureCode(cause))
			if code != "" && code == r.publishedFailure {
				// The stream repeated the failure it already reported.
				continue
			}
			r.publishedFailure = code
		}
		kept = append(kept, out)
	}
	return kept
}

// declare records the first failure the run declared. Later failures are
// diagnostics: the code a run ends with is the one it was first named with.
func (r *runObserver) declare(cause error, stopped bool) {
	if cause == nil || r.declared != nil {
		return
	}
	r.declared = cause
	r.stoppedAtDeclare = stopped
}

// declareHook records a failure the user-message hook refused. It is a setup
// failure under the run's policy, and the one a Web composer must be able to
// hand the draft back from.
func (r *runObserver) declareHook(cause error, stopped bool) {
	if cause == nil {
		return
	}
	r.hook = true
	r.declare(cause, stopped)
}

// facts assembles the ending the observer saw.
func (r *runObserver) facts(runCtx context.Context, phase runPhase) endingFacts {
	if r.phase != 0 {
		phase = r.phase
	}
	facts := endingFacts{
		Phase:            phase,
		RunCtx:           runCtx,
		Declared:         r.declared,
		Hook:             r.hook,
		StoppedAtDeclare: r.stoppedAtDeclare,
		Terminal:         r.terminal,
		Deferred:         r.deferred,
		Lost:             r.lost,
	}
	return facts
}

// consume reads the producer's events until its channel closes, publishing and
// queueing each one in order. It runs on its own goroutine, so a slow delivery
// end never delays publication: a run whose subscriber is slow must still
// reach the step commit barrier its engine waits on.
//
// A refused publication or a lost ownership does not end this loop. The
// producer keeps handing over events and must always be able to finish,
// whether its events are still being published or not.
func (s *Service) consume(
	ctx context.Context,
	spec runSpec,
	obs *runObserver,
	assets *outboundAssets,
	pub *runPublisherSet,
	queue *deliveryQueue,
	abort context.CancelCauseFunc,
	stopped func() bool,
	events <-chan native.StreamEvent,
) {
	// Publication outlives the run's execution context. A stopped run still
	// publishes the events its producer already emitted, and a publication must
	// not fail merely because the context it runs under was cancelled: the
	// session runtime's own gate races the cancellation, and losing that race
	// would report a stopped run as a publication failure.
	publishCtx := context.WithoutCancel(ctx)
	for ev := range events {
		published := obs.observe(ctx, s, spec.Req.BotID, assets, stopped(), ev)
		for _, out := range published {
			payload := marshalRunEvent(out)
			if publisher := pub.current(); publisher != nil {
				if err := publisher.publish(publishCtx, out, payload); err != nil {
					s.refusePublication(pub, obs, abort, err)
				}
			}
			queue.enqueue(RunEvent{Event: out, Payload: payload})
		}
	}
}

// refusePublication records what a refused publication means for the run and
// ends its execution. Ownership loss leaves the run to its new owner; any
// other refusal is the run's failure, and the publisher is retired so the run
// does not fail twice.
//
// The cancellation carries publishRefused, not the driver's stop marker: a
// refusal is a failure of this run, and a run nobody asked to stop must not
// report its ending as a stop.
func (s *Service) refusePublication(pub *runPublisherSet, obs *runObserver, abort context.CancelCauseFunc, err error) {
	pub.retire()
	if ownershipLost(err) {
		// Another owner holds this run. It is not a failure of this one, and
		// naming it as one would write history for a run this process can no
		// longer speak for.
		obs.lost = true
		s.logger.Warn("publishing run events lost ownership", slog.Any("error", err))
		if abort != nil {
			abort(err)
		}
		return
	}
	refused := publishRefused{runtimePublishError(err)}
	obs.declare(refused, false)
	obs.phase = phasePublish
	if abort != nil {
		abort(refused)
	}
}

// isHookRefusal reports whether a setup failure is the user-message hook
// refusing the turn. Two shapes reach here: the raw hook error, and the
// classified one a caller that already named it passes on. Both are the
// existing model's, so this reads them rather than inventing a third.
func isHookRefusal(err error) bool {
	return errors.Is(err, hooks.ErrDenied) ||
		apperror.CodeOf(err) == apperror.CodeHookUserMessageFailed
}

// marshalRunEvent renders one event once, so every consumer reads the same
// bytes; an error event leaves as its code and the catalog detail.
func marshalRunEvent(ev native.StreamEvent) json.RawMessage {
	data, err := json.Marshal(publicAgentStreamEvent(ev))
	if err != nil {
		return nil
	}
	return data
}
