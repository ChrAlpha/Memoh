package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/apperror"
)

var _ turn.Service = (*Service)(nil)

// SetAllowedTeam restricts the service to a single team. The in-process
// runtime's database pool is session-bound to one team GUC, so commands
// for any other team must fail closed (turn.ErrTeamNotServed) instead of
// silently operating on the bound team's data. The composition root
// injects the self-hosted singleton team; a hosted multi-team runtime
// replaces this with request-scoped team binding.
func (s *Service) SetAllowedTeam(teamID string) {
	s.allowedTeam = teamID
}

// StartTurn validates the command, admits it durably, starts the underlying
// stream, and returns a handle whose Events/Errs mirror the application stream.
func (s *Service) StartTurn(ctx context.Context, cmd turn.StartTurnCommand) (turn.RunHandle, error) {
	if cmd.TeamID == "" {
		return nil, errors.New("turn: TeamID is required")
	}
	if s.allowedTeam != "" && cmd.TeamID != s.allowedTeam {
		return nil, fmt.Errorf("%w: %s", turn.ErrTeamNotServed, cmd.TeamID)
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
	context.AfterFunc(ctx, func() { stopRun(runContextEnding(context.Cause(ctx))) })
	cancel := func() { stopRun(errRunStopped) }

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

	spec := runSpec{
		Run: admittedRun{
			Handle:       admission.Handle,
			TurnID:       admission.TurnID,
			TurnPosition: &admission.TurnPosition,
			Mode:         string(cmd.Mode),
		},
		Req:      req,
		Delivery: handle,
		Input:    inputAccepted,
	}
	// The caller's own context is this run's stop signal: an inbound consumer
	// that went away, a transport that closed, or the channel's own /stop. The
	// driver names the ending from the run context's own cause, so this one
	// carries no separate stop channel.
	spec.Setup = func(ctx context.Context, req ChatRequest) (runPlan, error) {
		return s.chatSetup(ctx, req, spec)
	}

	go func() {
		defer cancel()
		s.drive(runCtx, spec)
	}()

	return handle, nil
}

func runtimeHistoryError(err error) error {
	if err == nil || apperror.CodeOf(err) != "" {
		return err
	}
	return apperror.Wrap(apperror.CodeSessionHistoryInconsistent, err, nil)
}

// runtimePublishError names a failure to publish the run's live state to the
// Session Runtime. History is not involved (Redis down, run ownership lost),
// so it must not read as a history fault.
func runtimePublishError(err error) error {
	if err == nil || apperror.CodeOf(err) != "" {
		return err
	}
	return apperror.Wrap(apperror.CodeSessionPublishFailed, err, nil)
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
