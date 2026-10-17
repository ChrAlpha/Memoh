package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/turn/turnwire"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/auth"
	"github.com/felinics/memoh/internal/errs"
)

// WebAdmitInput is one Web submission as the WebSocket names it.
type WebAdmitInput struct {
	BotID        string
	SessionID    string
	InvocationID string
	// InitiatorID is the account the submission was made under.
	InitiatorID string
	// Payload is the canonical form of what the client sent. Its bytes decide
	// whether a repeated invocation is the same submit or a conflicting one.
	Payload []byte
	// Admission builds what subscribers see once the run leaves admitting: a
	// send's request user turn, or the boundary a retry or edit replaces. It
	// runs after ownership is established, so its writes are fenced.
	Admission func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error)
	// QueueTurn builds the complete turn of a send that may wait, and the
	// identities its credentials are issued for when it starts. It is called only
	// when the send does wait: the session has a run in flight, or input queued
	// ahead of it. A submission that cannot wait as plain input, such as a retry,
	// an edit or a skill activation, leaves it nil and meets ErrSessionBusy.
	QueueTurn func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error)
}

// WebAdmission is the run a Web submission was admitted as. The run id, its
// turn and the cursor it became observable at come from the session runtime,
// which is what makes them the same for every subscriber of the session.
type WebAdmission struct {
	RunID        string
	TurnID       string
	TurnPosition int64
	// Cursor is where the run became observable. A duplicate reserved nothing
	// and has none: where the session stands is what a subscription answers.
	Cursor sessionruntime.Cursor
	// Duplicate means the invocation already had a run, owned here or
	// elsewhere, or already finished. Executing it again is the duplication
	// admission exists to prevent, so there is nothing to run.
	Duplicate bool
	// Queued means the submission was not run: it waits in the session's queue
	// behind the run in flight and the input queued before it, and starts a run of
	// its own in its turn. QueuedItemID names it in the queue. A Duplicate queued
	// admission is a submission the queue already held or had taken.
	Queued       bool
	QueuedItemID string

	ctx       context.Context
	stop      context.CancelCauseFunc
	handle    sessionruntime.RunHandle
	injectCh  chan turn.InjectMessage
	endActive func()
}

// AdmitWebRun admits a Web submission. The ledger arbitrates every new turn: a
// repeated invocation resolves to its original run, a session already running
// one refuses this (sessionruntime.ErrSessionBusy), and the winner receives the
// fencing token its writes carry.
//
// The run belongs to the session, not to the socket that sent it: a client
// that disconnects leaves the run executing for the session's other
// subscribers. Its context is therefore derived from the caller's without
// inheriting the caller's cancellation, and is stopped only by a routed abort,
// a lost ownership, or the caller's own context naming a reason.
func (s *Service) AdmitWebRun(ctx context.Context, in WebAdmitInput) (*WebAdmission, error) {
	if s.sessionRuntime == nil {
		return nil, errs.New("session runtime is not configured")
	}
	if queued, err := s.queueWebRunBehindInput(ctx, in); queued != nil || err != nil {
		return queued, err
	}
	endActive, err := s.activeTurns.Begin()
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

	admission, err := s.sessionRuntime.Admit(runCtx, sessionruntime.AdmitInput{
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
	if errors.Is(err, sessionruntime.ErrSessionBusy) && in.QueueTurn != nil && s.queue != nil {
		// The run in flight took the session between the queue check and the
		// admission: the send waits for it like every other.
		stopRun(outcome.ErrRunStopped)
		endActive()
		return s.enqueueWebRun(ctx, in)
	}
	if err != nil {
		stopRun(outcome.ErrRunStopped)
		endActive()
		return nil, err
	}
	if !admission.Started {
		stopRun(outcome.ErrRunStopped)
		endActive()
		return &WebAdmission{
			RunID:        admission.RunID,
			TurnID:       admission.TurnID,
			TurnPosition: admission.TurnPosition,
			Duplicate:    true,
		}, nil
	}
	return &WebAdmission{
		RunID:        admission.RunID,
		TurnID:       admission.TurnID,
		TurnPosition: admission.TurnPosition,
		Cursor:       admission.Cursor,
		ctx:          s.withAdmissionRuntimeFence(runCtx, admission),
		stop:         stopRun,
		handle:       admission.Handle,
		// The run reads steering from its queue; the channel only lets the
		// native loop accept direct injections the way every chat run does.
		injectCh:  make(chan turn.InjectMessage, 16),
		endActive: endActive,
	}, nil
}

// queueWebRunBehindInput answers a send that must wait for input already queued
// for its session: a message the queue already holds or has taken is answered as
// the repeat it is, and one that would pass queued input joins the queue behind
// it. It returns nil for a send that admits as usual.
func (s *Service) queueWebRunBehindInput(ctx context.Context, in WebAdmitInput) (*WebAdmission, error) {
	if in.QueueTurn == nil || s.queue == nil {
		return nil, nil
	}
	gate, err := s.queue.Gate(ctx, sessionruntime.Key{BotID: strings.TrimSpace(in.BotID), SessionID: strings.TrimSpace(in.SessionID)}, strings.TrimSpace(in.InvocationID))
	if err != nil {
		return nil, fmt.Errorf("check session queue: %w", err)
	}
	switch {
	case gate.Seen:
		return &WebAdmission{Queued: true, Duplicate: true}, nil
	case gate.Waiting:
		return s.enqueueWebRun(ctx, in)
	default:
		return nil, nil
	}
}

// enqueueWebRun puts a send in the session's queue. The queue is the server's
// to start from: it replays the stored command under credentials issued when it
// starts, and the run's subscribers are the session's, as for any run of the
// session.
func (s *Service) enqueueWebRun(ctx context.Context, in WebAdmitInput) (*WebAdmission, error) {
	queue, err := s.queueStore()
	if err != nil {
		return nil, err
	}
	cmd, claims, err := in.QueueTurn(ctx)
	if err != nil {
		return nil, err
	}
	cmd.IdempotencyKey = strings.TrimSpace(in.InvocationID)
	payload, err := turnwire.NewQueuedInput(cmd, claims, time.Now()).Marshal()
	if err != nil {
		return nil, fmt.Errorf("encode queued send: %w", err)
	}
	item, err := queue.Enqueue(ctx, sessionruntime.QueueEnqueue{
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
	s.kickPending(ctx, strings.TrimSpace(in.BotID), strings.TrimSpace(in.SessionID))
	return &WebAdmission{Queued: true, QueuedItemID: item.ID}, nil
}

// RunWebSend executes an admitted Web send. The draft is still the
// submitter's until execution starts: a failure before then writes nothing, so
// the composer can hand it back without duplicating a turn.
func (s *Service) RunWebSend(admission *WebAdmission, req runreq.ChatRequest, delivery RunDelivery) {
	spec := s.webRunSpec(admission, req, delivery, outcome.InputHeld)
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (runPlan, error) {
		return s.chatSetup(ctx, req, spec)
	}
	s.startWebRun(admission, spec)
}

// RunWebRetry executes an admitted retry of the session's latest turn.
func (s *Service) RunWebRetry(admission *WebAdmission, input RetryLatestMessageInput, delivery RunDelivery) {
	s.startWebRun(admission, s.retryRunSpec(admission, input, delivery))
}

// RunWebEdit executes an admitted edit of the session's latest turn.
func (s *Service) RunWebEdit(admission *WebAdmission, input EditLatestMessageInput, delivery RunDelivery) {
	s.startWebRun(admission, s.editRunSpec(admission, input, delivery))
}

// webRunSpec is the part of a Web run every Web entry shares: the run
// admission named, and the request wired to it.
func (*Service) webRunSpec(admission *WebAdmission, req runreq.ChatRequest, delivery RunDelivery, input outcome.InputOwnership) runSpec {
	position := admission.TurnPosition
	req.RunID = admission.RunID
	req.RunHandle = admission.handle
	req.TurnID = admission.TurnID
	req.TurnPosition = &position
	req.InjectCh = admission.injectCh
	req.QueueSteerEnabled = admission.injectCh != nil
	return runSpec{
		Run: admittedRun{
			Handle:       admission.handle,
			TurnID:       admission.TurnID,
			TurnPosition: &position,
			Mode:         sessionmode.Chat,
		},
		Req:      req,
		Delivery: delivery,
		Input:    input,
	}
}

// startWebRun drives an admitted Web run on its own goroutine. Compaction of
// the session waits for the run: compacting history a run is still appending
// to would summarize a turn that is not finished.
func (s *Service) startWebRun(admission *WebAdmission, spec runSpec) {
	releaseCompaction := s.DeferSessionCompaction(spec.Req.BotID, spec.Req.ThreadID, admission.RunID)
	go func() {
		defer admission.endActive()
		defer admission.stop(outcome.ErrRunStopped)
		defer close(admission.injectCh)
		defer releaseCompaction()
		s.drive(admission.ctx, spec)
	}()
}
