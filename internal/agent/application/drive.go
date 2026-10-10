package application

import (
	"context"
	"sync"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// runEventBuffer is the capacity of the producer→consumer channel. It matches
// the native engine's own buffer so a burst of deltas crosses without the
// producer ever waiting on publication.
const runEventBuffer = 64

// runPublisherSet holds a run's publisher, which the consumer retires once a
// publication was refused. The driver and the consumer share one value, so a
// publisher that already failed is not called again for the terminal event.
type runPublisherSet struct {
	mu  sync.Mutex
	pub runPublisher
}

func (s *runPublisherSet) current() runPublisher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pub
}

func (s *runPublisherSet) retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pub = publisherRefused{}
}

// drive executes one run from the moment it is admitted to the moment its
// terminal state is written. It is the single driver for every entry: Web,
// channel, follow-up, retry, edit, decision continuation, and schedule. Each
// entry decides only what runSpec carries; the ordering below is the same for
// all of them.
//
// The run has three goroutines: this one, the consumer that publishes and
// queues the producer's events, and the delivery queue that hands them to the
// caller. Only this one writes history.
func (s *Service) drive(ctx context.Context, spec runSpec) runResult {
	if spec.Delivery == nil {
		spec.Delivery = noDelivery{}
	}
	runCtx := ctx
	// The execution context carries the stop the driver was asked for, and the
	// run context's own cause when the run ends for a reason outside it — an
	// ownership revoke, a budget. Without that the two would be told apart by
	// which context a caller happened to read.
	execCtx, stopExec := context.WithCancelCause(runCtx)
	defer stopExec(nil)
	stopInherit := context.AfterFunc(runCtx, func() { stopExec(context.Cause(runCtx)) })
	defer stopInherit()

	pub := &runPublisherSet{pub: s.publisherFor(spec)}
	obs := newRunObserver()
	assets := newOutboundAssets()
	spec.Req.OutboundAssetCollector = assets.collected
	queue := startDeliveryQueue(runCtx, spec.Delivery)
	stopWatch := watchRunStop(execCtx, stopExec, spec)
	defer stopWatch.stop()

	plan, setupErr := spec.Setup(execCtx)
	if setupErr != nil {
		if isHookRefusal(setupErr) {
			obs.declareHook(setupErr, runStopped(execCtx))
		} else {
			obs.declare(setupErr, runStopped(execCtx))
		}
		return s.settleRun(execCtx, spec, plan, pub, obs, queue, phaseSetup, nil)
	}
	spec.Req = plan.ReqFor(spec.Req)

	// The idle watchdog bounds model and tool silence. It cancels the execution
	// context with its own sentinel, which the failure policy reads as a
	// timeout rather than as a stop.
	idleCtx, idle := s.withStreamIdleTimeout(execCtx, plan.IdleEffort)
	defer idle.Stop()
	obs.idle = idle

	events := make(chan native.StreamEvent, runEventBuffer)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		s.consume(execCtx, spec, obs, assets, pub, queue, stopExec, func() bool { return runStopped(execCtx) }, events)
	}()

	ending := plan.Exec.execute(idleCtx, func(ev native.StreamEvent) {
		// The producer is offered events, never blocked on the consumer: a
		// closed consumer channel is how it learns the consumer is gone, which
		// is what keeps a runtime whose events nobody publishes from hanging.
		select {
		case events <- ev:
		case <-consumerDone:
		}
	})
	close(events)
	<-consumerDone
	stopWatch.stop()
	if idle.DidFire() && obs.declared == nil {
		obs.declare(context.Cause(idleCtx), false)
	}

	return s.settleRun(execCtx, spec, plan, pub, obs, queue, phaseExecution, &ending)
}

// settleRun names how a run ended, writes the history that ending owns, and
// publishes the run's terminal state. One path serves every ending, so the
// order — history first, terminal second — cannot differ between entries.
func (s *Service) settleRun(
	runCtx context.Context,
	spec runSpec,
	plan runPlan,
	pub *runPublisherSet,
	obs *runObserver,
	queue *deliveryQueue,
	phase runPhase,
	ending *execEnding,
) runResult {
	facts := obs.facts(runCtx, phase)
	if ending != nil {
		facts.Terminal = ending.Terminal
		facts.Deferred = ending.Deferred
	}
	// An executor that named its own failure keeps that name: it is the one
	// that knows where the failure came from. A bare cause is named from the
	// stage it was reported in. Either way the failure counts as declared, so
	// the stop and ownership rules see it in the right order.
	var named *RunFailure
	if ending != nil {
		named = ending.Failure
		if ending.Cause != nil && named == nil {
			code, args := runFailureCode(ending.Cause, ending.Phase, false)
			named = &RunFailure{Code: code, Args: args, Cause: ending.Cause, Phase: ending.Phase}
		}
	}
	if named != nil && facts.Declared == nil {
		facts.Declared = named.Cause
		facts.Phase = named.Phase
		facts.StoppedAtDeclare = runStopped(runCtx)
	}
	end, failure := nameRunEnd(facts)
	if named != nil && end == endFailed {
		end, failure = endFailed, named
	}

	// A stop and a lost run write no history: nothing is being reported, and
	// another owner names a lost run's outcome.
	var summary commitSummary
	switch {
	case end == endStopped || end == endOwnershipLost:
		// Nothing is being reported, and another owner names a lost run.
	case ending != nil && ending.Commit != nil:
		row := wantsFailureRow(spec.Input, end, failure, facts.Hook, spec.Replacement != nil)
		summary, facts.Commit = ending.Commit(context.WithoutCancel(runCtx), failure, row)
		if facts.Commit != nil {
			// The write failed, and that failure is now the run's: it cannot be
			// resumed from a state that was never written. This deliberately
			// does not re-read stopping — a stop found now would contradict the
			// failure row that was just written.
			end, failure = nameRunEnd(facts)
		}
	case failure != nil && wantsFailureRow(spec.Input, end, failure, facts.Hook, spec.Replacement != nil):
		// A run that failed before it had an executor to commit through — its
		// Setup, or its plan's context — still owes the failure its history.
		// The row carries the origin when the user-message hook is why it is
		// there, which is what lets a later retry recognize the turn.
		persisted, err := s.persistFailureRow(context.WithoutCancel(runCtx), spec.Req, resolvedContext{}, *failure, failureOrigin(facts.Hook))
		if err != nil {
			facts.Commit = runtimeHistoryError(err)
			end, failure = nameRunEnd(facts)
		} else {
			summary.Persisted = persisted
		}
	}
	if summary.Persisted == nil {
		summary.Persisted = []messagepkg.Message{}
	}

	// The run's context lifecycle terminal carries the context the run
	// assembled, and is written before the terminal state so the diagnostic
	// row and the run's outcome describe the same ending. A run another owner
	// ended has none.
	if plan.Lifecycle != nil && end != endOwnershipLost {
		plan.Lifecycle(lifecycleCause(end, failure))
	}

	// Publishing follows the history write: a subscriber must not see a run
	// complete before its output is durable.
	if end != endOwnershipLost {
		if pubErr := s.publishRunOutcome(runCtx, pub, queue, obs, ending, end, failure, facts.Commit); pubErr != nil {
			// The run's own terminal publication was refused. Ownership loss
			// is the one refusal that changes the outcome: another owner names
			// this run, so nothing may be written for it.
			if ownershipLost(pubErr) {
				end, failure = endOwnershipLost, nil
			} else if failure == nil {
				end, failure = endFailed, &RunFailure{
					Code:  codeFromError(pubErr, phasePublish, false),
					Args:  apperror.ArgsOf(pubErr),
					Cause: pubErr, Phase: phasePublish,
				}
			}
		}
	}
	if ending != nil && ending.Cleanup != nil {
		ending.Cleanup(context.WithoutCancel(runCtx))
	}

	// Everything published is delivered, or the caller is gone.
	queue.closeAndWait()
	spec.Delivery.Settle()
	terminal := s.finishDrivenRun(runCtx, spec.Run, end, failure)
	spec.Delivery.Ended(context.WithoutCancel(runCtx), RunEnded{
		State:     terminal.State,
		ErrorCode: terminal.ErrorCode,
		ErrorArgs: terminal.ErrorArgs,
		Failure:   publicRunFailure(failure),
	})

	return runResult{
		End:       end,
		Failure:   failure,
		Terminal:  terminal,
		Summary:   summary,
		Persisted: summary.Persisted,
	}
}

// publishRunOutcome publishes the run's failure and its terminal event. The
// terminal is withheld when the ending's history write failed: the run's
// durable outcome is then whatever the terminal write records, and publishing
// a completion the history does not have is the failure this ordering exists
// to prevent.
func (s *Service) publishRunOutcome(
	runCtx context.Context,
	pub *runPublisherSet,
	queue *deliveryQueue,
	obs *runObserver,
	ending *execEnding,
	end runEnd,
	failure *RunFailure,
	commitErr error,
) error {
	if failure != nil && obs.publishedFailure != string(failure.Code) {
		if err := s.emitRunEvent(runCtx, pub, queue, failureStreamEvent(*failure)); err != nil {
			return err
		}
	}
	if ending == nil || ending.Terminal == nil || commitErr != nil {
		return nil
	}
	return s.emitRunEvent(runCtx, pub, queue, stampRunTerminal(*ending.Terminal, end, failure))
}

// stampRunTerminal states how the run ended on its terminal event. The marker
// is what tells the session runtime the outcome was named by the run's owner,
// so it does not re-derive one from the last error its stream saw.
func stampRunTerminal(terminal native.StreamEvent, end runEnd, failure *RunFailure) native.StreamEvent {
	terminal.OwnerNamed = true
	if failure != nil {
		terminal.Code = string(failure.Code)
		terminal.Args = failure.Args
		if terminal.Type == event.AgentAbort && terminal.EndReason == "" {
			terminal.EndReason = event.EndFailed
		}
		return terminal
	}
	terminal.Code = ""
	terminal.Args = nil
	if terminal.EndReason != "" {
		return terminal
	}
	switch end {
	case endCompleted:
		terminal.EndReason = event.EndCompleted
	case endParked:
		terminal.EndReason = event.EndAwaitingDecision
	case endStopped:
		terminal.EndReason = event.EndCanceled
	}
	return terminal
}

// emitRunEvent publishes one event raised after the producer stopped. It uses
// a detached context: a stopped run's tail still reaches its subscribers, which
// is how they learn the run stopped.
func (*Service) emitRunEvent(runCtx context.Context, pub *runPublisherSet, queue *deliveryQueue, ev native.StreamEvent) error {
	payload := marshalRunEvent(ev)
	var publishErr error
	if publisher := pub.current(); publisher != nil {
		if err := publisher.publish(context.WithoutCancel(runCtx), ev, payload); err != nil {
			publishErr = err
		}
	}
	// The event still reaches the caller: whatever the subscribers saw, the
	// caller that started this run is told how it ended.
	queue.enqueue(RunEvent{Event: ev, Payload: payload})
	return publishErr
}

// failureOrigin names why a failure row is in history. Only the user-message
// hook produces an origin: those rows are dropped from the model's context
// before they reach it, so a rejected send is visible to the user without
// becoming context for the next one.
func failureOrigin(hook bool) string {
	if hook {
		return messagepkg.HistoryFailureOriginUserMessageHook
	}
	return ""
}

// lifecycleCause is what the run's context lifecycle records: the failure's own
// cause when there is one, and otherwise the reason the run ended.
func lifecycleCause(end runEnd, failure *RunFailure) error {
	if failure != nil {
		return failure.Cause
	}
	switch end {
	case endStopped:
		return errRunStopped
	case endOwnershipLost:
		return sessionruntime.ErrRunOwnershipLost
	default:
		return nil
	}
}

// publicRunFailure is the failure as the delivery end reports it: a public
// error whose text is its code, marked as already recorded because the run's
// own result record carries it, or nothing when the run did not fail.
func publicRunFailure(f *RunFailure) error {
	if f == nil {
		return nil
	}
	return f.Recorded()
}

// runStopWatch ends a run when something outside it says to stop. Only the
// entry's own signal is translated into the driver's marker: an ownership
// revoke or a budget expiry arrives through the run context instead, and
// carries its own cause.
type runStopWatch struct {
	done chan struct{}
	once func()
}

func watchRunStop(execCtx context.Context, stop context.CancelCauseFunc, spec runSpec) *runStopWatch {
	done := make(chan struct{})
	var once sync.Once
	watch := &runStopWatch{
		done: done,
		once: func() { once.Do(func() { close(done) }) },
	}
	if spec.StopCh == nil {
		return watch
	}
	// A signal already delivered when the run starts is a stop before it ran:
	// waiting for the watcher goroutine to be scheduled would let the run
	// execute and report a failure instead.
	select {
	case <-spec.StopCh:
		stop(errRunStopped)
		return watch
	default:
	}
	go func() {
		select {
		case <-spec.StopCh:
			stop(errRunStopped)
		case <-done:
		case <-execCtx.Done():
		}
	}()
	return watch
}

func (w *runStopWatch) stop() {
	if w == nil {
		return
	}
	w.once()
}

// endString names a runEnd for a log line.
func (e runEnd) String() string {
	switch e {
	case endCompleted:
		return "completed"
	case endParked:
		return "parked"
	case endStopped:
		return "stopped"
	case endFailed:
		return "failed"
	case endOwnershipLost:
		return "ownership_lost"
	default:
		return "unknown"
	}
}
