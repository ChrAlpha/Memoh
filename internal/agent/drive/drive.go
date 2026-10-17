package drive

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/felinics/memoh/internal/agent/event"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
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
	pub RunPublisher
}

func (s *runPublisherSet) current() RunPublisher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pub
}

func (s *runPublisherSet) retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pub = publisherRefused{}
}

// Drive executes one run from the moment it is admitted to the moment its
// terminal state is written. It is the single driver for every entry: Web,
// channel, follow-up, retry, edit, decision continuation, discuss, resume, and
// schedule. Each entry decides only what RunSpec carries; the ordering below is
// the same for all of them, and so is the run's turn span, which is why it is
// opened here and not by an entry.
//
// The run has three goroutines: this one, the consumer that publishes and
// queues the producer's events, and the delivery queue that hands them to the
// caller. Only this one writes history.
func (d *Driver) Drive(ctx context.Context, spec RunSpec) RunResult {
	ctx, endTurn := startTurnSpan(ctx, spec.Req)
	if spec.Delivery == nil {
		spec.Delivery = NoDelivery{}
	}
	runCtx := ctx
	// The execution context carries the stop the driver raises itself — a
	// publication refused, a panic — and every reason the run context ended
	// with, because it is a child of it. A stop is written on the run context
	// by whoever raises it (the entry's own signal, the session runtime's
	// routed abort), so the marker arrives here already named.
	execCtx, stopExec := context.WithCancelCause(runCtx)
	defer stopExec(nil)

	pub := &runPublisherSet{pub: d.publisherFor(spec)}
	obs := newRunObserver()
	assets := newOutboundAssets()
	queue := startDeliveryQueue(runCtx, spec.Delivery)

	// The driver finishes the request's wiring before Setup builds the run
	// around it: the collector is per run and the entry cannot build it.
	setupReq := spec.Req
	setupReq.OutboundAssetCollector = assets.collected

	// Everything up to the run's ending runs inside one recover: a run is other
	// people's code as far as this driver is concerned (an entry's Setup, a
	// runtime's producer, a delivery end), and one of them panicking must end
	// this run rather than the process — the caller is blocked on this run's
	// delivery, and a dead process takes every other session with it. The panic
	// is recorded as the run's failure and settlement runs as usual, so the
	// caller is answered instead of abandoned.
	phase := outcome.PhaseSetup
	plan, ending := func() (plan RunPlan, ending *ExecEnding) {
		defer func() {
			if r := recover(); r != nil {
				d.logPanickedRun(execCtx, spec, phase, r)
				obs.declare(panickedRunCause(r), outcome.RunStopped(execCtx))
			}
		}()

		var setupErr error
		plan, setupErr = spec.Setup(execCtx, setupReq)
		if setupErr != nil {
			if isHookRefusal(setupErr) {
				obs.declareHook(setupErr, outcome.RunStopped(execCtx))
			} else {
				obs.declare(setupErr, outcome.RunStopped(execCtx))
			}
			return plan, nil
		}
		// The plan's request is the run's from here on; the collector rides on
		// it because Setup was handed the wired request.
		spec.Req = plan.ReqFor(setupReq)

		// The idle watchdog bounds model and tool silence. It cancels the
		// execution context with its own sentinel, which the failure policy
		// reads as a timeout rather than as a stop.
		idleCtx, idle := d.withStreamIdleTimeout(execCtx, plan.IdleEffort)
		defer idle.Stop()
		obs.idle = idle

		events := make(chan native.StreamEvent, runEventBuffer)
		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			// A panic in the consumer must not leave the producer waiting on
			// consumerDone for a goroutine that is gone.
			defer func() {
				if r := recover(); r != nil {
					d.logPanickedRun(execCtx, spec, outcome.PhaseExecution, r)
					stopExec(panickedRunCause(r))
				}
			}()
			d.consume(execCtx, spec, obs, assets, pub, queue, stopExec, func() bool { return outcome.RunStopped(execCtx) }, events)
		}()
		// The producer is offered events and the consumer is collected
		// afterwards, whether or not the producer returned normally.
		defer func() {
			close(events)
			<-consumerDone
		}()

		phase = outcome.PhaseExecution
		done := plan.Exec.Execute(idleCtx, func(ev native.StreamEvent) {
			// The watchdog is fed on the producer's side, before the event is
			// offered to the consumer: it measures silence between events the
			// runtime produced, not how fast a slow subscriber drains them.
			idle.Observe(ev)
			// The producer is offered events, never blocked on the consumer: a
			// closed consumer channel is how it learns the consumer is gone,
			// which is what keeps a runtime whose events nobody publishes from
			// hanging.
			select {
			case events <- ev:
			case <-consumerDone:
			}
		})
		if idle.DidFire() && obs.declared == nil {
			obs.declare(context.Cause(idleCtx), false)
		}
		return plan, &done
	}()

	result := d.settleRun(execCtx, spec, plan, pub, obs, queue, phase, ending)
	endTurn(runSpanCause(result))
	return result
}

// panickedRunCause is how a panic inside a run is reported to its failure
// policy: the catalog's catch-all, because a panic names nothing a user could
// act on. The panic value stays behind it as the private cause, for the run's
// result record and its span.
func panickedRunCause(r any) error {
	return apperror.Wrap(apperror.CodeInternal, fmt.Errorf("agent run panicked: %v", r), nil)
}

// logPanickedRun records a recovered panic with the stack that caused it. A
// panic is always a bug, and the stack is the only thing that says which.
func (d *Driver) logPanickedRun(ctx context.Context, spec RunSpec, phase outcome.RunPhase, r any) {
	d.logger.ErrorContext(ctx, "agent run panicked",
		slog.String("phase", phaseName(phase)),
		slog.String("bot_id", spec.Req.BotID),
		slog.String("session_id", spec.Req.ThreadID),
		slog.String("run_id", spec.Run.Handle.RunID),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())),
	)
}

// logStoppedCommitFailure records a stopped run's history write that failed.
// Nothing else reports it: the run ended as a stop, so it has no failure for
// the result record to carry.
func (d *Driver) logStoppedCommitFailure(ctx context.Context, spec RunSpec, err error) {
	d.logger.ErrorContext(ctx, "stopped run's history was not written",
		slog.String("bot_id", spec.Req.BotID),
		slog.String("session_id", spec.Req.ThreadID),
		slog.String("run_id", spec.Run.Handle.RunID),
		slog.Any("error", err))
}

func phaseName(phase outcome.RunPhase) string {
	switch phase {
	case outcome.PhaseSetup:
		return "setup"
	case outcome.PhaseExecution:
		return "execution"
	case outcome.PhaseCommit:
		return "commit"
	case outcome.PhasePublish:
		return "publish"
	default:
		return "unknown"
	}
}

// settleRun names how a run ended, writes the history that ending owns, and
// publishes the run's terminal state. One path serves every ending, so the
// order — history first, terminal second — cannot differ between entries.
func (d *Driver) settleRun(
	runCtx context.Context,
	spec RunSpec,
	plan RunPlan,
	pub *runPublisherSet,
	obs *runObserver,
	queue *deliveryQueue,
	phase outcome.RunPhase,
	ending *ExecEnding,
) RunResult {
	facts := obs.facts(runCtx, phase)
	if ending != nil {
		facts.Terminal = ending.Terminal
		facts.Deferred = ending.Deferred
	}
	// An executor that named its own failure keeps that name: it is the one
	// that knows where the failure came from. A bare cause is named from the
	// stage it was reported in. Either way the failure counts as declared, so
	// the stop and ownership rules see it in the right order.
	var named *outcome.RunFailure
	if ending != nil {
		named = ending.Failure
		if ending.Cause != nil && named == nil {
			code, args := outcome.RunFailureCode(ending.Cause, ending.Phase, false)
			named = &outcome.RunFailure{Code: code, Args: args, Cause: ending.Cause, Phase: ending.Phase}
		}
	}
	if named != nil && facts.Declared == nil {
		facts.Declared = named.Cause
		facts.Phase = named.Phase
		facts.StoppedAtDeclare = outcome.RunStopped(runCtx)
	}
	end, failure := outcome.NameRunEnd(facts)
	if named != nil && end == outcome.EndFailed {
		end, failure = outcome.EndFailed, named
	}

	// A lost run writes no history: another owner names its outcome. A stop
	// reports nothing and leaves no failure, but it keeps what the user already
	// saw, so the executor writes that.
	var summary CommitSummary
	switch {
	case end == outcome.EndOwnershipLost:
		// Another owner names a lost run.
	case end == outcome.EndStopped:
		if ending != nil && ending.CommitStopped != nil {
			var err error
			summary, err = ending.CommitStopped(context.WithoutCancel(runCtx))
			if err != nil {
				// The run was stopped, so it reports no failure: its terminal
				// names the stop. A transcript that could not be written is
				// found in the log.
				d.logStoppedCommitFailure(runCtx, spec, err)
			}
		}
	case ending != nil && ending.Commit != nil:
		row := outcome.WantsFailureRow(spec.Input, end, failure, facts.Hook, spec.Replacement != nil)
		summary, facts.Commit = ending.Commit(context.WithoutCancel(runCtx), failure, row)
		if facts.Commit != nil {
			// The write failed, and that failure is now the run's: it cannot be
			// resumed from a state that was never written. This deliberately
			// does not re-read stopping — a stop found now would contradict the
			// failure row that was just written.
			end, failure = outcome.NameRunEnd(facts)
		}
	case failure != nil && outcome.WantsFailureRow(spec.Input, end, failure, facts.Hook, spec.Replacement != nil):
		// A run that failed before it had an executor to commit through — its
		// Setup, or its plan's context — still owes the failure its history.
		// The row carries the origin when the user-message hook is why it is
		// there, which is what lets a later retry recognize the turn.
		persisted, err := d.persist.FailureRow(context.WithoutCancel(runCtx), spec.Req, runreq.ResolvedContext{}, *failure, failureOrigin(facts.Hook))
		if err != nil {
			facts.Commit = outcome.RuntimeHistoryError(err)
			end, failure = outcome.NameRunEnd(facts)
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
	// ended has none, and a parked run has not ended: the continuation that
	// resumes it writes the row.
	if plan.Lifecycle != nil && end != outcome.EndOwnershipLost && end != outcome.EndParked {
		plan.Lifecycle(lifecycleCause(end, failure))
	}

	// Publishing follows the history write: a subscriber must not see a run
	// complete before its output is durable.
	if end != outcome.EndOwnershipLost {
		if pubErr := d.publishRunOutcome(runCtx, pub, queue, obs, ending, end, failure, facts.Commit); pubErr != nil {
			// The run's own terminal publication was refused. Ownership loss
			// is the one refusal that changes the outcome: another owner names
			// this run, so nothing may be written for it.
			if outcome.OwnershipLost(pubErr) {
				end, failure = outcome.EndOwnershipLost, nil
			} else if failure == nil {
				end, failure = outcome.EndFailed, &outcome.RunFailure{
					Code:  outcome.CodeFromError(pubErr, outcome.PhasePublish, false),
					Args:  apperror.ArgsOf(pubErr),
					Cause: pubErr, Phase: outcome.PhasePublish,
				}
			}
		}
	}
	if ending != nil && ending.Cleanup != nil {
		ending.Cleanup(context.WithoutCancel(runCtx))
	}

	// Everything published is delivered, or the caller is gone.
	queue.closeAndWait()
	// The assets the caller's own outbound path produced are linked here, after
	// its last event was handed over: before that they may still be on their
	// way, and the run's own history write has already linked what the run
	// produced. The two lists overlap, and linking the same asset twice is an
	// upsert on its message and content hash.
	if reported := reportedRunAssets(spec.Delivery); len(reported) > 0 {
		d.persist.LinkOutboundAssets(context.WithoutCancel(runCtx), spec.Req.BotID, spec.Req.ThreadID, runreq.OutboundAssetRefsToMessageRefs(reported))
	}
	spec.Delivery.Settle()
	terminal := d.FinishRun(runCtx, spec.Run, fromEnd(end, failure))
	spec.Delivery.Ended(context.WithoutCancel(runCtx), RunEnded{
		State:     terminal.State,
		ErrorCode: terminal.ErrorCode,
		ErrorArgs: terminal.ErrorArgs,
		Failure:   PublicRunFailure(failure),
	})

	return RunResult{
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
func (d *Driver) publishRunOutcome(
	runCtx context.Context,
	pub *runPublisherSet,
	queue *deliveryQueue,
	obs *runObserver,
	ending *ExecEnding,
	end outcome.RunEnd,
	failure *outcome.RunFailure,
	commitErr error,
) error {
	if failure != nil && obs.publishedFailure != string(failure.Code) {
		if err := d.emitRunEvent(runCtx, pub, queue, outcome.FailureStreamEvent(*failure)); err != nil {
			return err
		}
	}
	if ending != nil && ending.Terminal != nil && commitErr == nil {
		if err := d.emitRunEvent(runCtx, pub, queue, stampRunTerminal(*ending.Terminal, end, failure)); err != nil {
			return err
		}
	}
	// What the publisher keeps open for the run ends with the run's last event.
	// A refusal here is a refused publication like any other.
	if publisher := pub.current(); publisher != nil {
		return publisher.close(context.WithoutCancel(runCtx), end)
	}
	return nil
}

// stampRunTerminal states how the run ended on its terminal event. The marker
// is what tells the session runtime the outcome was named by the run's owner,
// so it does not re-derive one from the last error its stream saw.
func stampRunTerminal(terminal native.StreamEvent, end outcome.RunEnd, failure *outcome.RunFailure) native.StreamEvent {
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
	case outcome.EndCompleted:
		terminal.EndReason = event.EndCompleted
	case outcome.EndParked:
		terminal.EndReason = event.EndAwaitingDecision
	case outcome.EndStopped:
		terminal.EndReason = event.EndCanceled
	}
	return terminal
}

// emitRunEvent publishes one event raised after the producer stopped. It uses
// a detached context: a stopped run's tail still reaches its subscribers, which
// is how they learn the run stopped.
func (*Driver) emitRunEvent(runCtx context.Context, pub *runPublisherSet, queue *deliveryQueue, ev native.StreamEvent) error {
	payload := MarshalRunEvent(ev)
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
func lifecycleCause(end outcome.RunEnd, failure *outcome.RunFailure) error {
	if failure != nil {
		return failure.Cause
	}
	switch end {
	case outcome.EndStopped:
		return outcome.ErrRunStopped
	case outcome.EndOwnershipLost:
		return sessionruntime.ErrRunOwnershipLost
	default:
		return nil
	}
}

// PublicRunFailure is the failure as the delivery end reports it: a public
// error whose text is its code, marked as already recorded because the run's
// own result record carries it, or nothing when the run did not fail.
func PublicRunFailure(f *outcome.RunFailure) error {
	if f == nil {
		return nil
	}
	return f.Recorded()
}

// reportedRunAssets are the asset references the delivery end collected on the
// caller's behalf, if it collects any. A delivery end that has none is not
// asked for them.
func reportedRunAssets(delivery RunDelivery) []runreq.OutboundAssetRef {
	reporter, ok := delivery.(reportedAssetsSource)
	if !ok {
		return nil
	}
	return reporter.ReportedAssets()
}
