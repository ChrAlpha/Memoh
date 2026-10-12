package application

import (
	"context"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/errs"
)

// WebAdmitInput is one Web submission as the WebSocket names it.
type WebAdmitInput struct {
	BotID        string
	SessionID    string
	InvocationID string
	// Payload is the canonical form of what the client sent. Its bytes decide
	// whether a repeated invocation is the same submit or a conflicting one.
	Payload []byte
	// Admission builds what subscribers see once the run leaves admitting: a
	// send's request user turn, or the boundary a retry or edit replaces. It
	// runs after ownership is established, so its writes are fenced.
	Admission func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error)
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
	endActive, err := s.activeTurns.begin()
	if err != nil {
		return nil, err
	}
	runCtx, stopRun := context.WithCancelCause(context.WithoutCancel(ctx))
	context.AfterFunc(ctx, func() { stopRun(runContextEnding(context.Cause(ctx))) })
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
		Payload:      in.Payload,
		Execution: sessionruntime.Execution{
			Admission: builder,
			// A routed abort is someone stopping this run, which the driver
			// names from the marker it leaves.
			Cancel: func() { stopRun(errRunStopped) },
			// Revoking ownership cancels with its own cause: a run taken away
			// must leave no durable trace, which a stopped run does.
			OwnershipCancel: stopRun,
		},
	})
	if err != nil {
		stopRun(errRunStopped)
		endActive()
		return nil, err
	}
	if !admission.Started {
		stopRun(errRunStopped)
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

// RunWebSend executes an admitted Web send. The draft is still the
// submitter's until execution starts: a failure before then writes nothing, so
// the composer can hand it back without duplicating a turn.
func (s *Service) RunWebSend(admission *WebAdmission, req ChatRequest, delivery RunDelivery) {
	spec := s.webRunSpec(admission, req, delivery, inputHeld)
	spec.Setup = func(ctx context.Context, req ChatRequest) (runPlan, error) {
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
func (*Service) webRunSpec(admission *WebAdmission, req ChatRequest, delivery RunDelivery, input inputOwnership) runSpec {
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
		defer admission.stop(errRunStopped)
		defer close(admission.injectCh)
		defer releaseCompaction()
		ctx, endTurn := startTurnSpan(admission.ctx, spec.Req)
		endTurn(runSpanCause(s.drive(ctx, spec)))
	}()
}

// runSpanCause is what a driven run's turn span records: the failure's public
// error, a cancellation for a stop (a run someone stopped is not a failure),
// and nothing for a run that completed or parked on a decision.
func runSpanCause(result runResult) error {
	switch result.End {
	case endFailed:
		if result.Failure != nil {
			return result.Failure.Public()
		}
		return errs.New("agent run failed")
	case endStopped:
		return context.Canceled
	case endOwnershipLost:
		return sessionruntime.ErrRunOwnershipLost
	default:
		return nil
	}
}
