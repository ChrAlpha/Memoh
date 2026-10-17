package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
)

var _ turn.Service = (*Service)(nil)

// StartTurn validates the command, admits it durably, starts the underlying
// stream, and returns a handle whose Events/Errs mirror the application stream.
func (s *Service) StartTurn(ctx context.Context, cmd turn.StartTurnCommand) (turn.RunHandle, error) {
	if cmd.TeamID == "" {
		return nil, errors.New("turn: TeamID is required")
	}
	if s.allowedTeam != "" && cmd.TeamID != s.allowedTeam {
		return nil, fmt.Errorf("%w: %s", turn.ErrTeamNotServed, cmd.TeamID)
	}

	if err := s.admitAfterQueue(ctx, cmd); err != nil {
		return nil, err
	}
	// The earlier messages of a queued batch are rows of their own, written
	// ahead of the run's turn. A failure here leaves no run behind, so the batch
	// is started again as it was.
	if err := s.persistPrecedingMessages(ctx, cmd); err != nil {
		return nil, err
	}

	// A run names its own ending, so its context is derived from the caller's
	// without inheriting it: a cancelled parent fixes its children's cause, and
	// what the caller's ending means to this run is a stop, not the plain
	// cancellation it carries. The caller going away, a transport closing, the
	// channel being told /stop — for this run they are one thing, and the driver
	// names the ending from the marker they leave. The context is cancellable by
	// cause so a lost owner lease can stop execution with a reason, instead of a
	// superseded owner racing the fence it can no longer pass.
	runCtx, stopRun := context.WithCancelCause(context.WithoutCancel(ctx))
	context.AfterFunc(ctx, func() { stopRun(outcome.RunContextEnding(context.Cause(ctx))) })
	cancel := func() { stopRun(outcome.ErrRunStopped) }

	injectCh := make(chan turn.InjectMessage, 16)
	admission, err := s.admitTurnRun(runCtx, cmd, cancel, stopRun, injectCh)
	if err != nil {
		cancel()
		return nil, err
	}
	runCtx = s.withAdmissionRuntimeFence(runCtx, admission)

	events := make(chan turn.Event, 16)
	errs := make(chan error, 1)
	handle := newTurnRun(runCtx, admission.RunID, cmd.TeamID, cmd.ThreadID, events, errs, injectCh, stopRun)

	req := chatRequestFromCommand(cmd)
	req.RunID = admission.RunID
	req.RunHandle = admission.Handle
	req.TurnID = admission.TurnID
	req.TurnPosition = &admission.TurnPosition
	req.InjectCh = injectCh
	req.QueueSteerEnabled = injectCh != nil

	spec := drive.RunSpec{
		Run: drive.AdmittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         string(cmd.Mode),
		},
		Req:      req,
		Delivery: handle,
		Input:    outcome.InputAccepted,
	}
	// The caller's own context is this run's stop signal: an inbound consumer
	// that went away, a transport that closed, or the channel's own /stop. The
	// driver names the ending from the run context's own cause, so this one
	// carries no separate stop channel.
	spec.Setup = func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
		return s.chatSetup(ctx, req, spec)
	}

	go func() {
		defer cancel()
		s.driver.Drive(runCtx, spec)
	}()

	return handle, nil
}

// admitAfterQueue is the session queue's gate on a new turn. A turn whose
// message the queue already holds or has taken was delivered before, and is
// answered like any redelivery. A turn that would pass input queued ahead of
// it is busy: the input waits its turn in the queue, in order. The queue's own
// admission is the one command that neither waits nor is counted.
func (s *Service) admitAfterQueue(ctx context.Context, cmd turn.StartTurnCommand) error {
	if cmd.NoDefer || s.queue == nil || strings.TrimSpace(cmd.ThreadID) == "" {
		return nil
	}
	gate, err := s.queue.Gate(ctx, sessionruntime.Key{BotID: cmd.BotID, SessionID: cmd.ThreadID}, cmd.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("check session queue: %w", err)
	}
	switch {
	case gate.Seen:
		return fmt.Errorf("%w: %s", turn.ErrDuplicateTurn, cmd.IdempotencyKey)
	case gate.Waiting:
		return fmt.Errorf("%w: thread %s has queued input", turn.ErrSessionBusy, cmd.ThreadID)
	}
	return nil
}

// RespondToolApproval resumes a turn deferred on tool approval.
func (s *Service) RespondToolApproval(ctx context.Context, input turn.ToolApprovalResponse, eventCh chan<- json.RawMessage) error {
	converted := toolApprovalInputFromResponse(input)
	if handled, err := s.routeToolApprovalResponse(ctx, converted, eventCh); handled || err != nil {
		return err
	}
	return s.respondToolApproval(ctx, converted, eventCh)
}

// RespondUserInput resumes a turn deferred on ask_user.
func (s *Service) RespondUserInput(ctx context.Context, input turn.UserInputResponse, eventCh chan<- json.RawMessage) error {
	converted := userInputInputFromResponse(input)
	if handled, err := s.routeUserInputResponse(ctx, converted, eventCh); handled || err != nil {
		return err
	}
	return s.respondUserInput(ctx, converted, eventCh)
}

// StopTurn routes cancellation to the durable owner even after the channel
// stream has closed, using the same application entry point as the web Stop.
func (s *Service) StopTurn(ctx context.Context, cmd turn.StopCommand) (bool, error) {
	if cmd.TeamID == "" || (s.allowedTeam != "" && cmd.TeamID != s.allowedTeam) {
		return false, turn.ErrTeamNotServed
	}
	if cmd.BotID == "" || cmd.ThreadID == "" {
		return false, errors.New("bot and thread are required")
	}
	if s.decisionRuntime == nil {
		return false, nil
	}
	snapshot, err := s.decisionRuntime.Snapshot(ctx, cmd.BotID, cmd.ThreadID)
	if err != nil {
		return false, err
	}
	if snapshot.CurrentRunView == nil {
		return false, nil
	}
	return s.AbortRuntimeRun(ctx, cmd.BotID, cmd.ThreadID, snapshot.CurrentRunView.RunID, "")
}
