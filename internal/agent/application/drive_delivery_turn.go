package application

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/felinics/memoh/internal/agent/turn"
)

// turnRun is the turn port's end of a driven run: the delivery end the driver
// hands its events to, and the handle its caller steers. It carries the two
// facts the port needs beyond the event payload — who the run belongs to, and
// the counter that makes the stream ordered.
type turnRun struct {
	ctx      context.Context // the run's context: ended means the caller is gone
	id       string
	teamID   string
	threadID string

	events chan turn.Event
	errs   chan error
	inject chan turn.InjectMessage
	// cancel stops the run this handle speaks for. The caller stopping a run is
	// a stop rather than a cancellation nobody asked for, which is why it
	// carries the driver's own marker.
	cancel context.CancelCauseFunc

	mu            sync.Mutex
	seq           int64
	injectStopped bool
	injectClosed  bool
	assets        []turn.OutboundAssetRef
	// callerGone records that the caller's context had already ended when the
	// run's last event was delivered. It is read rather than the context
	// itself: the driver's terminal write cancels the run context, so by the
	// time Ended runs, the same context would report every finished run as one
	// whose caller left.
	callerGone bool
}

func newTurnRun(
	ctx context.Context,
	runID, teamID, threadID string,
	events chan turn.Event,
	errs chan error,
	inject chan turn.InjectMessage,
	cancel context.CancelCauseFunc,
) *turnRun {
	return &turnRun{
		ctx:      ctx,
		id:       runID,
		teamID:   teamID,
		threadID: threadID,
		events:   events,
		errs:     errs,
		inject:   inject,
		cancel:   cancel,
	}
}

// Deliver implements RunDelivery. It reports false once the caller is gone,
// which the run's own context ending is the only signal of: a caller that is
// merely slow keeps its events.
func (t *turnRun) Deliver(_ context.Context, ev RunEvent) bool {
	if t.ctx.Err() != nil {
		return false
	}
	event := turn.Event{
		RunID:    t.id,
		TeamID:   t.teamID,
		ThreadID: t.threadID,
		Seq:      t.nextSeq(),
		Kind:     string(ev.Event.Type),
		Payload:  ev.Payload,
	}
	select {
	case t.events <- event:
		return true
	case <-t.ctx.Done():
		return false
	}
}

// Settle implements RunDelivery. Direct injection stops here, before the run's
// terminal write: the session runtime may still append to the run it is
// finishing, but this handle may not.
func (t *turnRun) Settle() {
	t.stopInject()
	t.mu.Lock()
	t.callerGone = t.ctx.Err() != nil
	t.mu.Unlock()
}

// Ended implements RunDelivery. It reports the run's failure to the caller,
// hands over the terminal record, and closes the event channels.
func (t *turnRun) Ended(ctx context.Context, end RunEnded) {
	defer t.closeInject()

	if end.Failure != nil {
		select {
		case t.errs <- end.Failure:
		case <-ctx.Done():
		}
	}
	t.mu.Lock()
	callerGone := t.callerGone
	t.mu.Unlock()
	if end.State == "" || callerGone {
		// The run parked, lost its owner, or never got a durable record; or the
		// caller stopped reading before the run ended. Either way there is no
		// terminal to report, exactly as before run_terminal existed.
		return
	}
	payload, err := json.Marshal(turn.RunTerminal{
		Type:      turn.EventRunTerminal,
		State:     end.State,
		ErrorCode: end.ErrorCode,
		ErrorArgs: end.ErrorArgs,
	})
	if err != nil {
		return
	}
	// The run has ended by the time this is sent, so a caller that stopped
	// reading only loses the event; it must not hold the driver open.
	timer := time.NewTimer(terminalEventTimeout)
	defer timer.Stop()
	select {
	case t.events <- turn.Event{
		RunID:    t.id,
		TeamID:   t.teamID,
		ThreadID: t.threadID,
		Seq:      t.nextSeq(),
		Kind:     turn.EventRunTerminal,
		Payload:  payload,
	}:
	case <-timer.C:
	}
}

// Inject implements turn.RunHandle. A message for a run whose execution has
// stopped is answered as such rather than dropped, so a caller can tell a
// rejected steer from an accepted one.
func (t *turnRun) Inject(ctx context.Context, msg turn.InjectMessage) error {
	t.mu.Lock()
	if t.injectStopped || t.injectClosed {
		t.mu.Unlock()
		return errRunAlreadyFinished
	}
	t.mu.Unlock()
	if t.inject == nil {
		return errRunAlreadyFinished
	}
	select {
	case t.inject <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return t.ctx.Err()
	}
}

// Cancel implements turn.RunHandle. The caller stopping a run is the run's
// stop signal, not an anonymous cancellation: the driver names the ending from
// it, and a failure the run already declared keeps its own name instead.
func (t *turnRun) Cancel() {
	if t.cancel != nil {
		t.cancel(errRunStopped)
	}
}

// StopInject implements turn.RunHandle.
func (t *turnRun) StopInject() {
	t.stopInject()
}

// RunID implements turn.RunHandle.
func (t *turnRun) RunID() string { return t.id }

// Events implements turn.RunHandle.
func (t *turnRun) Events() <-chan turn.Event { return t.events }

// Errs implements turn.RunHandle.
func (t *turnRun) Errs() <-chan error { return t.errs }

// AddOutboundAssets implements turn.RunHandle: the assets the delivery end
// reports back join the ones the run's own events carried.
func (t *turnRun) AddOutboundAssets(refs []turn.OutboundAssetRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.assets = append(t.assets, refs...)
}

// reportedAssets are the references the delivery end reported.
func (t *turnRun) reportedAssets() []turn.OutboundAssetRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]turn.OutboundAssetRef(nil), t.assets...)
}

// ReportsRunTerminal implements turn.TerminalReporter. Every run this handle
// speaks for is admitted, so every one of them ends with a terminal record or
// an explicit reason it has none.
func (*turnRun) ReportsRunTerminal() bool { return true }

// nextSeq hands out the stream's ordering counter. The first event is sequence
// one: the port's consumers index events from one, not from zero.
func (t *turnRun) nextSeq() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	return t.seq
}

func (t *turnRun) stopInject() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.injectStopped = true
}

// closeInject closes the inject channel once, after direct injections have
// stopped and before the event channels close.
func (t *turnRun) closeInject() {
	t.mu.Lock()
	alreadyClosed := t.injectClosed
	t.injectStopped = true
	t.injectClosed = true
	t.mu.Unlock()

	if alreadyClosed {
		return
	}
	if t.inject != nil {
		close(t.inject)
	}
	close(t.events)
	close(t.errs)
}

// errRunAlreadyFinished is what a steer gets once the run it names has stopped
// executing. It reads as the port's own vocabulary rather than as the driver's.
var errRunAlreadyFinished = errors.New("turn: run already finished")
