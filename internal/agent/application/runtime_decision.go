package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/decision"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/db"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/workspace"
)

// ResolveRuntimeDecision reads the authoritative decision row before any live
// owner lookup. Terminal rows are returned too: the router needs to distinguish
// a known, already-decided request from an unfenced ACP/MCP request.
func (s *Service) ResolveRuntimeDecision(ctx context.Context, commandType, decisionID string) (sessionruntime.DecisionTarget, error) {
	if s == nil || s.queries == nil {
		return sessionruntime.DecisionTarget{}, errors.New("runtime decision store is not configured")
	}
	id, err := db.ParseUUID(decisionID)
	if err != nil {
		return sessionruntime.DecisionTarget{}, fmt.Errorf("%w: %w", sessionruntime.ErrDecisionNotFound, err)
	}
	switch commandType {
	case sessionruntime.CommandToolApprovalResponse:
		row, err := s.queries.GetToolApprovalRequest(ctx, id)
		if err != nil {
			return sessionruntime.DecisionTarget{}, runtimeDecisionReadError(err)
		}
		return toolApprovalDecisionTarget(row), nil
	case sessionruntime.CommandUserInputResponse:
		row, err := s.queries.GetUserInputRequest(ctx, id)
		if err != nil {
			return sessionruntime.DecisionTarget{}, runtimeDecisionReadError(err)
		}
		return userInputDecisionTarget(row), nil
	default:
		return sessionruntime.DecisionTarget{}, fmt.Errorf("unsupported runtime decision command %q", commandType)
	}
}

// PendingRuntimeDecisions resolves every durable decision that parked runID.
// It is used only by expired-owner recovery, where preserving the exact rows
// is required before advancing the run's fencing token — a turn can park on
// several approvals and user inputs at once, and dropping any of them here
// would supersede a decision the user can still answer.
func (s *Service) PendingRuntimeDecisions(ctx context.Context, runID string) ([]sessionruntime.DecisionTarget, error) {
	if s == nil || s.queries == nil {
		return nil, errors.New("runtime decision store is not configured")
	}
	id, err := db.ParseUUID(runID)
	if err != nil {
		return nil, err
	}
	approvals, err := s.queries.ListPendingToolApprovalsByRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read pending tool approvals for run: %w", err)
	}
	inputs, err := s.queries.ListPendingUserInputsByRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read pending user inputs for run: %w", err)
	}
	out := make([]sessionruntime.DecisionTarget, 0, len(approvals)+len(inputs))
	for _, approval := range approvals {
		out = append(out, toolApprovalDecisionTarget(approval))
	}
	for _, input := range inputs {
		out = append(out, userInputDecisionTarget(input))
	}
	// Recovery decides by session runtime whether the parked run is
	// resumable at all; every pending decision here shares one session.
	if len(out) > 0 && s.sessionService != nil {
		sess, sessErr := s.sessionService.Get(ctx, out[0].SessionID)
		if sessErr != nil {
			return nil, fmt.Errorf("read waiting-decision session runtime: %w", sessErr)
		}
		for i := range out {
			out[i].SessionRuntime = sess.RuntimeType
		}
	}
	return out, nil
}

func runtimeDecisionReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return sessionruntime.ErrDecisionNotFound
	}
	return err
}

func toolApprovalDecisionTarget(row dbsqlc.ToolApprovalRequest) sessionruntime.DecisionTarget {
	return sessionruntime.DecisionTarget{
		Type: sessionruntime.CommandToolApprovalResponse, ID: decisionUUIDString(row.ID),
		BotID: decisionUUIDString(row.BotID), SessionID: decisionUUIDString(row.SessionID),
		RunID: decisionUUIDString(row.RunID), TurnID: decisionUUIDString(row.TurnID),
		Status: row.Status, FencingToken: pgInt64(row.RuntimeFencingToken),
		InlineDecision: isCapabilityManagementApproval(toolapproval.Request{ToolName: row.ToolName}),
		ControlID:      pgText(row.ResponseControlID), PayloadHash: pgText(row.ResponsePayloadHash),
	}
}

func userInputDecisionTarget(row dbsqlc.UserInputRequest) sessionruntime.DecisionTarget {
	return sessionruntime.DecisionTarget{
		Type: sessionruntime.CommandUserInputResponse, ID: decisionUUIDString(row.ID),
		BotID: decisionUUIDString(row.BotID), SessionID: decisionUUIDString(row.SessionID),
		RunID: decisionUUIDString(row.RunID), TurnID: decisionUUIDString(row.TurnID),
		Status: row.Status, FencingToken: pgInt64(row.RuntimeFencingToken),
		ControlID: pgText(row.ResponseControlID), PayloadHash: pgText(row.ResponsePayloadHash),
	}
}

func decisionUUIDString(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	return uuid.UUID(value.Bytes).String()
}

func pgInt64(value pgtype.Int8) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func pgText(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func (s *Service) routeToolApprovalResponse(ctx context.Context, input ToolApprovalResponseInput, eventCh chan<- WSStreamEvent) (bool, error) {
	if s == nil || s.decisionRuntime == nil {
		return false, nil
	}
	if input.ControlID == "" {
		input.ControlID = implicitDecisionControlID(sessionruntime.CommandToolApprovalResponse, firstNonEmpty(input.ExplicitID, input.ApprovalID))
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return true, err
	}
	result, err := s.decisionRuntime.StreamDecisionResponse(ctx, sessionruntime.DecisionResponse{
		ControlID: input.ControlID, Type: sessionruntime.CommandToolApprovalResponse,
		DecisionID: firstNonEmpty(input.ExplicitID, input.ApprovalID),
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
			raw, _ := json.Marshal(agentFailureStreamEvent(err))
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

func (s *Service) routeUserInputResponse(ctx context.Context, input UserInputResponseInput, eventCh chan<- WSStreamEvent) (bool, error) {
	if s == nil || s.decisionRuntime == nil {
		return false, nil
	}
	if input.ControlID == "" {
		input.ControlID = implicitDecisionControlID(sessionruntime.CommandUserInputResponse, firstNonEmpty(input.ExplicitID, input.UserInputID))
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return true, err
	}
	result, err := s.decisionRuntime.StreamDecisionResponse(ctx, sessionruntime.DecisionResponse{
		ControlID: input.ControlID, Type: sessionruntime.CommandUserInputResponse,
		DecisionID: firstNonEmpty(input.ExplicitID, input.UserInputID),
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
			raw, _ := json.Marshal(agentFailureStreamEvent(err))
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
		var input UserInputResponseInput
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

		committed, err := s.CommitUserInputResponse(ctx, input)
		if err != nil {
			runCancel()
			return err
		}
		committed.runID = command.RunID
		committed.runHandle = runHandle
		s.publishCommittedRuntimeDecision(runCtx, command, native.StreamEvent{
			Type:        native.EventUserInputRequest,
			ToolName:    committed.request.ToolName,
			ToolCallID:  committed.request.ToolCallID,
			UserInputID: committed.request.ID,
			ShortID:     committed.request.ShortID,
			Status:      committed.request.Status,
			Input:       committed.request.Input,
			Metadata:    userinput.DeferredMetadata(committed.request),
		})
		if committed.isExternalAgent || committed.ackOnly {
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
		var input ToolApprovalResponseInput
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

		committed, err := s.CommitToolApprovalResponse(ctx, input)
		if err != nil {
			runCancel()
			return err
		}
		committed.runID = command.RunID
		committed.runHandle = runHandle
		s.publishCommittedRuntimeDecision(runCtx, command, native.StreamEvent{
			Type:           native.EventToolApprovalRequest,
			ToolName:       committed.request.ToolName,
			InlineDecision: committed.usesDecisionWaiter,
			ToolCallID:     committed.request.ToolCallID,
			ApprovalID:     committed.request.ID,
			ShortID:        committed.request.ShortID,
			Status:         committed.request.Status,
			Input:          committed.request.ToolInput,
			Metadata:       approvalResultMetadata(committed.request),
		})
		if committed.usesDecisionWaiter || committed.ackOnly {
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
	Req ChatRequest
	// Setup stores the answer as the parked call's result and resolves the
	// resumed run. It runs inside the run, so its failure is the run's.
	//
	// The request arrives as an argument for the same reason it does in
	// runSpec.Setup: the entry built this continuation before the driver ran,
	// so the request it closed over has no asset collector, and a resumed run
	// that used it would write its outbound assets unlinked.
	Setup func(ctx context.Context, req ChatRequest) (runPlan, error)
}

// driveRoutedContinuation drives the run an answer the session runtime routed
// to this owner resumes. The run is the one the decision parked; its events
// reach the session projection and the answering command's output log, and
// nobody is waiting on this goroutine for them.
func (s *Service) driveRoutedContinuation(runCtx context.Context, command sessionruntime.Command, handle sessionruntime.RunHandle, cont decisionContinuation) {
	ctx, release := continuationContext(workspace.WithWorkspaceTarget(runCtx, cont.Req.WorkspaceTargetID))
	defer release()
	s.drive(ctx, runSpec{
		Run: admittedRun{Handle: handle, TurnID: handle.TurnID, Mode: sessionmode.Chat},
		Req: cont.Req,
		Setup: func(ctx context.Context, req ChatRequest) (runPlan, error) {
			// The answer may arrive before the run that parked has finished
			// writing the call it answers; its result must not land first.
			if err := s.decisionRuntime.WaitDecisionContinuationReady(ctx, command); err != nil {
				return runPlan{}, err
			}
			return cont.Setup(ctx, req)
		},
		Delivery:  noDelivery{},
		Input:     inputAccepted,
		Publisher: &decisionOutputPublisher{sink: s.decisionRuntime, handle: handle, command: command},
	})
}

// driveUnroutedContinuation drives the run an answer resumes when no session
// runtime routed it, so no run was admitted for it: the caller that submitted
// the answer follows the run on eventCh, or nobody does when it is nil.
//
// A caller without an event stream learns a failure only from the return
// value; one with a stream already received it as the run's error event.
func (s *Service) driveUnroutedContinuation(ctx context.Context, cont decisionContinuation, eventCh chan<- WSStreamEvent) error {
	ctx, release := continuationContext(workspace.WithWorkspaceTarget(ctx, cont.Req.WorkspaceTargetID))
	defer release()
	var delivery RunDelivery = noDelivery{}
	if eventCh != nil {
		delivery = channelDelivery{ch: eventCh}
	}
	result := s.drive(ctx, runSpec{
		Req:      cont.Req,
		Setup:    cont.Setup,
		Delivery: delivery,
		Input:    inputAccepted,
	})
	if eventCh != nil {
		return nil
	}
	return publicRunFailure(result.Failure)
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
			cause = errRunStopped
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

// nativeContinuationPlan resolves the built-in agent run that resumes after an
// answered decision. Its steps commit through the live queue the run was
// admitted with, so the resumed steps continue the parked run's numbering.
func (s *Service) nativeContinuationPlan(ctx context.Context, req ChatRequest, cfg native.RunConfig, modelID string) (runPlan, error) {
	rc := resolvedContext{runConfig: cfg, model: models.GetResponse{ID: modelID}}
	committer, err := s.bindQueueContinuation(ctx, &req, &cfg, rc)
	if err != nil {
		return runPlan{}, err
	}
	timing := newReasoningTimingTracker(nil)
	configureNativeReasoningTiming(&cfg, timing, committer)
	rc.runConfig = cfg
	return runPlan{
		Exec: &nativeExecutor{
			agent:     s.agent,
			cfg:       cfg,
			rc:        rc,
			req:       req,
			committer: committer,
			timing:    timing,
			service:   s,
		},
		Req:        req,
		IdleEffort: reasoningEffortForIdle(cfg),
		Lifecycle:  s.contextLifecycleTerminal(ctx, cfg),
	}, nil
}
