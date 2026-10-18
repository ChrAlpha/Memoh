package application

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/decision"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/workspace"
)

func (s *Service) routeToolApprovalResponse(ctx context.Context, input decide.ToolApprovalResponseInput, eventCh chan<- runreq.WSStreamEvent) (bool, error) {
	if s == nil || s.decisionRuntime == nil {
		return false, nil
	}
	if input.ControlID == "" {
		input.ControlID = implicitDecisionControlID(sessionruntime.CommandToolApprovalResponse, runreq.FirstNonEmpty(input.ExplicitID, input.ApprovalID))
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return true, err
	}
	result, err := s.decisionRuntime.StreamDecisionResponse(ctx, sessionruntime.DecisionResponse{
		ControlID: input.ControlID, Type: sessionruntime.CommandToolApprovalResponse,
		DecisionID: runreq.FirstNonEmpty(input.ExplicitID, input.ApprovalID),
		BotID:      input.BotID, SessionID: input.ThreadID, Payload: payload,
	}, eventCh)
	if errors.Is(err, sessionruntime.ErrDecisionNotFound) {
		return false, nil
	}
	if err != nil {
		if result.Applied && eventCh != nil {
			if s.logger != nil {
				s.logger.WarnContext(ctx, "accepted decision output interrupted", slog.Any("error", err))
			}
			raw, _ := json.Marshal(outcome.AgentFailureStreamEvent(err))
			select {
			case eventCh <- raw:
				return true, nil
			case <-ctx.Done():
				return true, ctx.Err()
			}
		}
		return true, err
	}
	if !result.Handled {
		return false, nil
	}
	if !result.Applied {
		return true, toolapproval.ErrAlreadyDecided
	}
	return true, nil
}

func (s *Service) routeUserInputResponse(ctx context.Context, input decide.UserInputResponseInput, eventCh chan<- runreq.WSStreamEvent) (bool, error) {
	if s == nil || s.decisionRuntime == nil {
		return false, nil
	}
	if input.ControlID == "" {
		input.ControlID = implicitDecisionControlID(sessionruntime.CommandUserInputResponse, runreq.FirstNonEmpty(input.ExplicitID, input.UserInputID))
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return true, err
	}
	result, err := s.decisionRuntime.StreamDecisionResponse(ctx, sessionruntime.DecisionResponse{
		ControlID: input.ControlID, Type: sessionruntime.CommandUserInputResponse,
		DecisionID: runreq.FirstNonEmpty(input.ExplicitID, input.UserInputID),
		BotID:      input.BotID, SessionID: input.ThreadID, Payload: payload,
	}, eventCh)
	if errors.Is(err, sessionruntime.ErrDecisionNotFound) {
		return false, nil
	}
	if err != nil {
		if result.Applied && eventCh != nil {
			if s.logger != nil {
				s.logger.WarnContext(ctx, "accepted decision output interrupted", slog.Any("error", err))
			}
			raw, _ := json.Marshal(outcome.AgentFailureStreamEvent(err))
			select {
			case eventCh <- raw:
				return true, nil
			case <-ctx.Done():
				return true, ctx.Err()
			}
		}
		return true, err
	}
	if !result.Handled {
		return false, nil
	}
	if !result.Applied {
		return true, userinput.ErrAlreadyDecided
	}
	return true, nil
}

func implicitDecisionControlID(commandType, decisionID string) string {
	return "implicit:" + commandType + ":" + decisionID
}

// handleRuntimeDecisionCommand commits on the routed-command deadline, then
// continues independently on the owning run. The command result therefore
// means "the decision was durably accepted", not "the model finished".
//
//nolint:contextcheck // the continuation is rooted in the owning run, not the acknowledgement request.
func (s *Service) handleRuntimeDecisionCommand(ctx context.Context, command sessionruntime.Command) error {
	if s == nil || s.decisionRuntime == nil {
		return errors.New("runtime decision handler is not configured")
	}
	runCtx, runCancel, runHandle, err := s.decisionRuntime.DecisionContinuationContext(command)
	if err != nil {
		return err
	}
	switch command.Type {
	case sessionruntime.CommandUserInputResponse:
		var input decide.UserInputResponseInput
		if err := json.Unmarshal(command.Payload, &input); err != nil {
			runCancel()
			return err
		}
		input.BotID = command.BotID
		input.ThreadID = command.SessionID
		input.UserInputID = command.TargetID
		input.ExplicitID = command.TargetID
		input.ReplyExternalMessageID = ""
		input.ChatToken = ""
		input.SuppressActivePromptAttach = true
		ctx = decision.WithResponseIdentity(ctx, decision.ResponseIdentity{
			ControlID: input.ControlID, PayloadHash: command.PayloadHash,
		})

		committed, err := s.decider.CommitUserInputResponse(ctx, input)
		if err != nil {
			runCancel()
			return err
		}
		committed = committed.ForRun(command.RunID, runHandle)
		answered := committed.Request()
		s.publishCommittedRuntimeDecision(runCtx, command, native.StreamEvent{
			Type:        native.EventUserInputRequest,
			ToolName:    answered.ToolName,
			ToolCallID:  answered.ToolCallID,
			UserInputID: answered.ID,
			ShortID:     answered.ShortID,
			Status:      answered.Status,
			Input:       answered.Input,
			Metadata:    userinput.DeferredMetadata(answered),
		})
		if committed.IsExternalAgent() || committed.AckOnly() {
			defer runCancel()
			s.ackInlineRuntimeDecision(runCtx, command)
			return nil
		}
		go func() {
			defer runCancel()
			s.driveRoutedContinuation(runCtx, command, runHandle, s.userInputContinuation(runCtx, committed, true))
		}()
		return nil
	case sessionruntime.CommandToolApprovalResponse:
		var input decide.ToolApprovalResponseInput
		if err := json.Unmarshal(command.Payload, &input); err != nil {
			runCancel()
			return err
		}
		input.BotID = command.BotID
		input.ThreadID = command.SessionID
		input.ApprovalID = command.TargetID
		input.ExplicitID = command.TargetID
		input.ReplyExternalMessageID = ""
		input.ChatToken = ""
		input.SuppressActivePromptAttach = true
		ctx = decision.WithResponseIdentity(ctx, decision.ResponseIdentity{
			ControlID: input.ControlID, PayloadHash: command.PayloadHash,
		})

		committed, err := s.decider.CommitToolApprovalResponse(ctx, input)
		if err != nil {
			runCancel()
			return err
		}
		committed = committed.ForRun(command.RunID, runHandle)
		approved := committed.Request()
		s.publishCommittedRuntimeDecision(runCtx, command, native.StreamEvent{
			Type:           native.EventToolApprovalRequest,
			ToolName:       approved.ToolName,
			InlineDecision: committed.UsesDecisionWaiter(),
			ToolCallID:     approved.ToolCallID,
			ApprovalID:     approved.ID,
			ShortID:        approved.ShortID,
			Status:         approved.Status,
			Input:          approved.ToolInput,
			Metadata:       decide.ApprovalResultMetadata(approved),
		})
		if committed.UsesDecisionWaiter() || committed.AckOnly() {
			defer runCancel()
			s.ackInlineRuntimeDecision(runCtx, command)
			return nil
		}
		go func() {
			defer runCancel()
			s.driveRoutedContinuation(runCtx, command, runHandle, s.toolApprovalContinuation(runCtx, committed, true))
		}()
		return nil
	default:
		runCancel()
		return errors.New("unsupported runtime decision command")
	}
}

// Inline runtimes consume the committed answer inside their original run.
// Close only this command's output; synthetic agent start/end events or a
// deferred continuation would race the original producer's lifecycle.
func (s *Service) ackInlineRuntimeDecision(ctx context.Context, command sessionruntime.Command) {
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.decisionRuntime.PublishDecisionOutput(ackCtx, command, 1, nil); err != nil && s.logger != nil {
		// The answer is already durable. An output transport failure must not
		// fail the independently running producer or undo the accepted answer.
		s.logger.WarnContext(ctx, "close inline decision output failed", slog.Any("error", err),
			slog.String("run_id", command.RunID), slog.String("decision_id", command.TargetID))
	}
}

// publishCommittedRuntimeDecision replaces the pending decision in the live
// projection before the command is acknowledged. PostgreSQL remains the
// correctness boundary: once Commit*Response succeeds the user's answer is
// accepted even if publishing the derived live view fails, so publication
// errors are logged and the continuation still runs.
func (s *Service) publishCommittedRuntimeDecision(ctx context.Context, command sessionruntime.Command, event native.StreamEvent) {
	if s == nil || s.decisionRuntime == nil {
		return
	}
	handle := sessionruntime.RunHandle{
		BotID:      command.BotID,
		SessionID:  command.SessionID,
		RunID:      command.RunID,
		Generation: command.Generation,
	}
	if _, err := s.decisionRuntime.HandleAgentEvent(ctx, handle, event); err != nil && s.logger != nil {
		s.logger.WarnContext(ctx, "publish committed runtime decision failed",
			slog.Any("error", err),
			slog.String("run_id", command.RunID),
			slog.String("decision_id", command.TargetID),
			slog.String("command_type", command.Type))
	}
}

// decisionContinuation is the run an answered decision resumes.
type decisionContinuation struct {
	// Req is the request the resumed run executes as. It names the turn the
	// decision parked, so a failure row the run leaves lands in that turn.
	Req runreq.ChatRequest
	// Setup stores the answer as the parked call's result and resolves the
	// resumed run. It runs inside the run, so its failure is the run's.
	//
	// The request arrives as an argument for the same reason it does in
	// runSpec.Setup: the entry built this continuation before the driver ran,
	// so the request it closed over has no asset collector, and a resumed run
	// that used it would write its outbound assets unlinked.
	Setup func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error)
}

// driveRoutedContinuation drives the run an answer the session runtime routed
// to this owner resumes. The run is the one the decision parked; its events
// reach the session projection and the answering command's output log, and
// nobody is waiting on this goroutine for them.
func (s *Service) driveRoutedContinuation(runCtx context.Context, command sessionruntime.Command, handle sessionruntime.RunHandle, cont decisionContinuation) {
	ctx, release := continuationContext(workspace.WithWorkspaceTarget(runCtx, cont.Req.WorkspaceTargetID))
	defer release()
	s.driver.Drive(ctx, drive.RunSpec{
		Run: drive.AdmittedRun{Handle: handle, TurnID: handle.TurnID, Mode: sessionmode.Chat},
		Req: cont.Req,
		Setup: func(ctx context.Context, req runreq.ChatRequest) (drive.RunPlan, error) {
			// The answer may arrive before the run that parked has finished
			// writing the call it answers; its result must not land first.
			if err := s.decisionRuntime.WaitDecisionContinuationReady(ctx, command); err != nil {
				return drive.RunPlan{}, err
			}
			return cont.Setup(ctx, req)
		},
		Delivery:  drive.NoDelivery{},
		Input:     outcome.InputAccepted,
		Publisher: drive.NewDecisionOutputPublisher(s.decisionRuntime, handle, command),
	})
}

// driveUnroutedContinuation drives the run an answer resumes when no session
// runtime routed it, so no run was admitted for it: the caller that submitted
// the answer follows the run on eventCh, or nobody does when it is nil.
//
// A caller without an event stream learns a failure only from the return
// value; one with a stream already received it as the run's error event.
func (s *Service) driveUnroutedContinuation(ctx context.Context, cont decisionContinuation, eventCh chan<- runreq.WSStreamEvent) error {
	ctx, release := continuationContext(workspace.WithWorkspaceTarget(ctx, cont.Req.WorkspaceTargetID))
	defer release()
	var delivery drive.RunDelivery = drive.NoDelivery{}
	if eventCh != nil {
		delivery = drive.NewChannelDelivery(eventCh)
	}
	result := s.driver.Drive(ctx, drive.RunSpec{
		Req:      cont.Req,
		Setup:    cont.Setup,
		Delivery: delivery,
		Input:    outcome.InputAccepted,
	})
	if eventCh != nil {
		return nil
	}
	return drive.PublicRunFailure(result.Failure)
}

// continuationContext is the context a decision continuation runs in. Its
// owner ends it by cancelling it: a routed run's commands stop when the run is
// aborted, and an unrouted run's caller cancels when it leaves. Both are a
// stop, which the driver only recognizes by its own marker, so a plain
// cancellation is carried over as one. A cancellation with a cause of its own,
// such as a revoked ownership, keeps that cause.
func continuationContext(ctx context.Context) (context.Context, func()) {
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	carry := func() {
		cause := context.Cause(ctx)
		if errors.Is(cause, context.Canceled) {
			cause = outcome.ErrRunStopped
		}
		cancel(cause)
	}
	stop := context.AfterFunc(ctx, carry)
	// AfterFunc runs its callback on a goroutine of its own. A context already
	// ended is carried over now, so the run does not start before it learns it
	// was stopped.
	if ctx.Err() != nil {
		carry()
	}
	return runCtx, func() {
		stop()
		cancel(context.Canceled)
	}
}

// continuer is what resuming an answered decision needs of the plan unit: the
// plan of the run that continues from it.
type continuer interface {
	Continue(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error)
}

// decidedContinuation is the run an answered decision resumes: the decided
// call's result is stored as the answer to the call the run parked on, and the
// model runs on from there. live says whether a caller receives the run's
// events as they happen.
func (s *Service) decidedContinuation(ctx context.Context, call decide.DecidedCall, live bool) decisionContinuation {
	requestMessageID, reqErr := s.transcript.ContinuationRequestMessageID(ctx, call.SessionID, call.RunHandle)
	req := runreq.ChatRequest{
		RunID:                   runreq.RunID(call.RunID),
		RunHandle:               call.RunHandle,
		BotID:                   call.BotID,
		ChatID:                  call.BotID,
		ThreadID:                call.SessionID,
		SourceChannelIdentityID: call.ChannelIdentityID,
		CurrentChannel:          call.SourcePlatform,
		ReplyTarget:             call.ReplyTarget,
		ConversationType:        call.ConversationType,
		UserMessagePersisted:    true,
		PersistedUserMessageID:  requestMessageID,
		// An answer stored as a tool result schedules no user-message memory
		// extraction.
		SkipMemoryExtraction: call.SkipMemoryExtraction,
		WorkspaceTargetID:    call.WorkspaceTargetID,
	}
	return decisionContinuation{
		Req: req,
		// The resumed run is the driver's request, which carries the per-run
		// asset collector. Everything else in it is what this entry built.
		Setup: func(ctx context.Context, runReq runreq.ChatRequest) (drive.RunPlan, error) {
			if reqErr != nil {
				return drive.RunPlan{}, reqErr
			}
			if err := call.Validate(); err != nil {
				return drive.RunPlan{}, err
			}
			return s.continuer.Continue(ctx, call, runReq, live)
		},
	}
}
