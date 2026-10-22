package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/apperror"
)

// Admission is the run a Web submission was admitted as, and implements
// agentapi.WebAdmission. The run id, its turn and the cursor it became
// observable at come from the session runtime, which is what makes them the
// same for every subscriber of the session.
type Admission struct {
	runID        string
	turnID       string
	turnPosition int64
	cursor       sessionruntime.Cursor
	duplicate    bool
	queued       bool
	queuedItemID string

	ctx       context.Context
	stop      context.CancelCauseFunc
	handle    sessionruntime.RunHandle
	injectCh  chan turn.InjectMessage
	endActive func()
}

var _ agentapi.WebAdmission = (*Admission)(nil)

func (a *Admission) RunID() string                 { return a.runID }
func (a *Admission) TurnID() string                { return a.turnID }
func (a *Admission) TurnPosition() int64           { return a.turnPosition }
func (a *Admission) Cursor() sessionruntime.Cursor { return a.cursor }
func (a *Admission) Duplicate() bool               { return a.duplicate }
func (a *Admission) Queued() bool                  { return a.queued }
func (a *Admission) QueuedItemID() string          { return a.queuedItemID }

// Context is the context the run executes under: derived from the caller's
// without inheriting its cancellation, and carrying the run's fence.
func (a *Admission) Context() context.Context { return a.ctx }

// Stop ends an admitted run with cause. The run's driver names its outcome from
// the cause; a caller that admitted a run and will not start it stops it here
// and releases it.
func (a *Admission) Stop(cause error) { a.stop(cause) }

// Release ends the count of this run as in flight, which a run's start does when
// the run ends. A caller that admitted a run and will not start it releases it.
func (a *Admission) Release() { a.endActive() }

// Admit admits a Web submission. The ledger arbitrates every new turn: a
// repeated invocation resolves to its original run, a session already running
// one refuses this (sessionruntime.ErrSessionBusy), and the winner receives the
// fencing token its writes carry.
//
// The run belongs to the session, not to the socket that sent it: a client
// that disconnects leaves the run executing for the session's other
// subscribers. Its context is therefore derived from the caller's without
// inheriting the caller's cancellation, and is stopped only by a routed abort,
// a lost ownership, or the caller's own context naming a reason.
func (r *Runner) Admit(ctx context.Context, in agentapi.WebAdmitInput) (*Admission, error) {
	if queued, err := r.queueBehindInput(ctx, in); queued != nil || err != nil {
		return queued, err
	}
	endActive, err := r.activeTurns.Begin()
	if err != nil {
		return nil, err
	}
	runCtx, stopRun := context.WithCancelCause(context.WithoutCancel(ctx))
	context.AfterFunc(ctx, func() { stopRun(outcome.RunContextEnding(context.Cause(ctx))) })
	builder := in.Admission
	if builder == nil {
		builder = func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		}
	}

	admission, err := r.runtime.Admit(runCtx, sessionruntime.AdmitInput{
		BotID:        strings.TrimSpace(in.BotID),
		SessionID:    strings.TrimSpace(in.SessionID),
		InvocationID: strings.TrimSpace(in.InvocationID),
		InitiatorID:  in.InitiatorID,
		Payload:      in.Payload,
		Execution: sessionruntime.Execution{
			Admission: builder,
			// A routed abort is someone stopping this run, which the driver
			// names from the marker it leaves.
			Cancel: func() { stopRun(outcome.ErrRunStopped) },
			// Revoking ownership cancels with its own cause: a run taken away
			// must leave no durable trace, which a stopped run does.
			OwnershipCancel: stopRun,
		},
	})
	if errors.Is(err, sessionruntime.ErrSessionBusy) && in.QueueTurn != nil && r.pending.Queueing() {
		// The run in flight took the session between the queue check and the
		// admission: the send waits for it like every other.
		stopRun(outcome.ErrRunStopped)
		endActive()
		return r.enqueueSend(ctx, in)
	}
	if err != nil {
		stopRun(outcome.ErrRunStopped)
		endActive()
		return nil, err
	}
	if !admission.Started {
		stopRun(outcome.ErrRunStopped)
		endActive()
		return &Admission{
			runID:        admission.RunID,
			turnID:       admission.TurnID,
			turnPosition: admission.TurnPosition,
			duplicate:    true,
		}, nil
	}
	return &Admission{
		runID:        admission.RunID,
		turnID:       admission.TurnID,
		turnPosition: admission.TurnPosition,
		cursor:       admission.Cursor,
		ctx:          r.admit.Fence(runCtx, admission),
		stop:         stopRun,
		handle:       admission.Handle,
		// The run reads steering from its queue; the channel only lets the
		// native loop accept direct injections the way every chat run does.
		injectCh:  make(chan turn.InjectMessage, 16),
		endActive: endActive,
	}, nil
}

// queueBehindInput answers a send that must wait for input already queued
// for its session: a message the queue already holds or has taken is answered as
// the repeat it is, and one that would pass queued input joins the queue behind
// it. It returns nil for a send that admits as usual.
func (r *Runner) queueBehindInput(ctx context.Context, in agentapi.WebAdmitInput) (*Admission, error) {
	if in.QueueTurn == nil {
		return nil, nil
	}
	gate, err := r.pending.Gate(ctx, sessionruntime.Key{BotID: strings.TrimSpace(in.BotID), SessionID: strings.TrimSpace(in.SessionID)}, strings.TrimSpace(in.InvocationID))
	if err != nil {
		return nil, fmt.Errorf("check session queue: %w", err)
	}
	switch {
	case gate.Seen:
		return &Admission{queued: true, duplicate: true}, nil
	case gate.Waiting:
		return r.enqueueSend(ctx, in)
	default:
		return nil, nil
	}
}

// enqueueSend puts a send in the session's queue. The queue is the server's
// to start from: it replays the stored command under credentials issued when it
// starts, and the run's subscribers are the session's, as for any run of the
// session.
func (r *Runner) enqueueSend(ctx context.Context, in agentapi.WebAdmitInput) (*Admission, error) {
	cmd, claims, err := in.QueueTurn(ctx)
	if err != nil {
		return nil, err
	}
	cmd.IdempotencyKey = strings.TrimSpace(in.InvocationID)
	payload, err := turnwire.NewQueuedInput(cmd, claims, time.Now()).Marshal()
	if err != nil {
		return nil, fmt.Errorf("encode queued send: %w", err)
	}
	item, err := r.pending.Enqueue(ctx, sessionruntime.QueueEnqueue{
		Key:          sessionruntime.Key{BotID: strings.TrimSpace(in.BotID), SessionID: strings.TrimSpace(in.SessionID)},
		Origin:       sessionruntime.QueueOriginFollowUp,
		InvocationID: strings.TrimSpace(in.InvocationID),
		MergeKey:     runreq.FirstNonEmpty(cmd.SourceChannelIdentityID, cmd.UserID),
		Payload:      payload,
	})
	switch {
	case errors.Is(err, sessionruntime.ErrQueueCapacityExceeded):
		return nil, apperror.New(apperror.CodeSessionQueueFull, nil)
	case errors.Is(err, sessionruntime.ErrQueueInvocationConflict):
		return nil, sessionruntime.ErrInvocationConflict
	case err != nil:
		return nil, err
	}
	// The run the send waits for may have ended before the row was written.
	r.pending.Kick(ctx, strings.TrimSpace(in.BotID), strings.TrimSpace(in.SessionID))
	return &Admission{queued: true, queuedItemID: item.ID}, nil
}

// RunSend executes an admitted Web send. The draft is still the
// submitter's until execution starts: a failure before then writes nothing, so
// the composer can hand it back without duplicating a turn.
func (r *Runner) RunSend(admission *Admission, req runreq.ChatRequest, delivery agentapi.RunDelivery) {
	spec := RunSpec(admission, req, delivery, outcome.InputHeld)
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		return r.plan.ChatSetup(ctx, req, spec)
	}
	r.Start(admission, spec)
}

// RunRetry executes an admitted retry of the session's latest turn.
func (r *Runner) RunRetry(admission *Admission, input agentapi.WebRetryInput, delivery agentapi.RunDelivery) {
	r.Start(admission, r.RetrySpec(admission, input, delivery))
}

// RunEdit executes an admitted edit of the session's latest turn.
func (r *Runner) RunEdit(admission *Admission, input agentapi.WebEditInput, delivery agentapi.RunDelivery) {
	r.Start(admission, r.EditSpec(admission, input, delivery))
}

// RunSpec is the part of a Web run every Web entry shares: the run
// admission named, and the request wired to it.
func RunSpec(admission *Admission, req runreq.ChatRequest, delivery agentapi.RunDelivery, input outcome.InputOwnership) drive.RunSpec {
	position := admission.turnPosition
	req.RunID = admission.runID
	req.RunHandle = admission.handle
	req.TurnID = admission.turnID
	req.TurnPosition = &position
	req.InjectCh = admission.injectCh
	req.QueueSteerEnabled = admission.injectCh != nil
	return drive.RunSpec{
		Run: drive.AdmittedRun{
			Handle:       admission.handle,
			TurnID:       admission.turnID,
			TurnPosition: &position,
			Mode:         sessionmode.Chat,
		},
		Req:      req,
		Delivery: delivery,
		Input:    input,
	}
}

// Start drives an admitted Web run on its own goroutine. Compaction of
// the session waits for the run: compacting history a run is still appending
// to would summarize a turn that is not finished.
func (r *Runner) Start(admission *Admission, spec drive.RunSpec) {
	releaseCompaction := r.compact.Defer(spec.Req.BotID, spec.Req.ThreadID, admission.runID)
	go func() {
		defer admission.endActive()
		defer admission.stop(outcome.ErrRunStopped)
		defer close(admission.injectCh)
		defer releaseCompaction()
		r.driver.Drive(admission.ctx, spec)
	}()
}
