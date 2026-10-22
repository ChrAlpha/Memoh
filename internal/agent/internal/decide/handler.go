package decide

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/sessionfacts"
	"github.com/felinics/memoh/internal/workspace"
)

// approvalHandlerParams is what the approval handler of one run needs to know
// about the run: the session and route a decision is requested on, and whether
// the run may park to wait for it.
type HandlerParams struct {
	BotID             string
	SessionID         string
	RouteID           string
	ChannelIdentityID string
	CurrentPlatform   string
	ReplyTarget       string
	ConversationType  string
	DecisionMode      sessionfacts.DecisionMode
}

func (d *Decider) ApprovalHandler(p HandlerParams) func(context.Context, sdk.ToolCall) (toolexec.ToolApprovalResult, error) {
	return func(ctx context.Context, call sdk.ToolCall) (toolexec.ToolApprovalResult, error) {
		if strings.TrimSpace(call.ToolName) == userinput.ToolNameAskUser {
			if err := userinput.ValidateAskUserInput(toolexec.ArgumentsValue(call.Input)); err != nil {
				// Let the tool's Execute handler return an instructional tool result
				// to the model instead of creating a fake pending request.
				return toolexec.ToolApprovalResult{Decision: toolexec.ToolApprovalDecisionApproved}, nil
			}
			if p.DecisionMode != sessionfacts.DecisionPark {
				return d.limitToolApprovalResult(toolexec.ToolApprovalResult{
					Decision: toolexec.ToolApprovalDecisionRejected,
					Reason:   "user input requested in a non-interactive session",
				}, call.ToolName), nil
			}
			// No ExpiresAt here: application requests have no in-process
			// waiter — the run pauses and resumes whenever the user answers,
			// even much later. Only waiter-backed (ACP/MCP) requests expire.
			req, err := d.userInput.CreatePending(ctx, userinput.CreatePendingInput{
				BotID:                        p.BotID,
				SessionID:                    p.SessionID,
				RouteID:                      p.RouteID,
				ChannelIdentityID:            p.ChannelIdentityID,
				RequestedByChannelIdentityID: p.ChannelIdentityID,
				ToolCallID:                   call.ToolCallID,
				ToolName:                     call.ToolName,
				Input:                        toolexec.ArgumentsValue(call.Input),
				SourcePlatform:               p.CurrentPlatform,
				ReplyTarget:                  p.ReplyTarget,
				ConversationType:             p.ConversationType,
				WorkspaceTargetID:            workspace.WorkspaceTargetFromContext(ctx),
			})
			if err != nil {
				return toolexec.ToolApprovalResult{}, err
			}
			if req.Status != userinput.StatusPending {
				return d.limitToolApprovalResult(toolexec.ToolApprovalResult{
					Decision:   toolexec.ToolApprovalDecisionRejected,
					ApprovalID: req.ID,
					Reason:     "ask_user request is already " + req.Status,
					Metadata:   userinput.DeferredMetadata(req),
				}, call.ToolName), nil
			}
			return toolexec.ToolApprovalResult{
				Decision:   toolexec.ToolApprovalDecisionDeferred,
				ApprovalID: req.ID,
				Metadata:   userinput.DeferredMetadata(req),
			}, nil
		}
		input := toolapproval.CreatePendingInput{
			BotID:                        p.BotID,
			SessionID:                    p.SessionID,
			RouteID:                      p.RouteID,
			ChannelIdentityID:            p.ChannelIdentityID,
			RequestedByChannelIdentityID: p.ChannelIdentityID,
			ToolCallID:                   call.ToolCallID,
			ToolName:                     call.ToolName,
			ToolInput:                    toolexec.ArgumentsValue(call.Input),
			SourcePlatform:               p.CurrentPlatform,
			ReplyTarget:                  p.ReplyTarget,
			ConversationType:             p.ConversationType,
			WorkspaceTargeted:            isWorkspaceTargetTool(call.ToolName),
			WorkspaceTargetID:            workspace.WorkspaceTargetFromContext(ctx),
		}
		_, forcedApproval := native.HookForcedApprovalReason(ctx)
		eval, err := d.toolApproval.EvaluatePolicy(ctx, input)
		if err != nil {
			if input.WorkspaceTargeted && errors.Is(err, workspace.ErrWorkspaceTargetNotFound) {
				requestedTargetID := ""
				if args, ok := toolexec.ArgumentsValue(call.Input).(map[string]any); ok {
					requestedTargetID = strings.TrimSpace(runreq.ReadAnyString(args["target_id"]))
				}
				d.logger.Warn("workspace tool target not found during approval",
					slog.String("bot_id", p.BotID),
					slog.String("tool_name", call.ToolName),
					slog.String("tool_call_id", call.ToolCallID),
					slog.String("requested_target_id", requestedTargetID),
				)
				// Twilight treats approval handler errors as fatal to the whole run.
				// A missing target is invalid model input, so let the tool execute
				// and return its normal, instructional error instead. This is safe
				// only for not-found targets: they cannot execute or bypass policy.
				return toolexec.ToolApprovalResult{Decision: toolexec.ToolApprovalDecisionApproved}, nil
			}
			return toolexec.ToolApprovalResult{}, err
		}
		input.ExecutionLocation = eval.ExecutionLocation
		locationMetadata := executionLocationResultMetadata(eval.ExecutionLocation)
		// The policy resolved the workspace target and pinned it on the
		// argument map it evaluated. The executor takes these arguments for
		// the call, so the tool runs where the policy looked and the persisted
		// call names that target (call.Input is a copy the handler cannot
		// mutate).
		var approvedInput *sdk.ToolArguments
		if input.WorkspaceTargeted {
			rewritten := toolexec.ArgumentsFromValue(input.ToolInput)
			approvedInput = &rewritten
		}
		if eval.Decision == toolapproval.DecisionDeny {
			return d.limitToolApprovalResult(toolexec.ToolApprovalResult{
				Decision: toolexec.ToolApprovalDecisionRejected,
				Reason:   toolapproval.PolicyDeniedReason,
				Metadata: locationMetadata,
			}, call.ToolName), nil
		}
		if eval.Decision == toolapproval.DecisionBypass && !forcedApproval {
			return toolexec.ToolApprovalResult{
				Decision: toolexec.ToolApprovalDecisionApproved,
				Metadata: locationMetadata,
				Input:    approvedInput,
			}, nil
		}
		if p.DecisionMode != sessionfacts.DecisionPark {
			reason := "tool execution requires approval, but this session type cannot request approval"
			rejected, err := d.toolApproval.CreateSystemRejected(ctx, input, reason)
			if err != nil {
				return toolexec.ToolApprovalResult{}, err
			}
			return d.limitToolApprovalResult(toolexec.ToolApprovalResult{
				Decision:   toolexec.ToolApprovalDecisionRejected,
				ApprovalID: rejected.ID,
				Reason:     reason,
				Metadata:   ApprovalResultMetadata(rejected),
			}, call.ToolName), nil
		}
		req, err := d.toolApproval.CreatePending(ctx, input)
		if err != nil {
			return toolexec.ToolApprovalResult{}, err
		}
		return toolexec.ToolApprovalResult{
			Decision:   toolexec.ToolApprovalDecisionDeferred,
			ApprovalID: req.ID,
			Metadata:   ApprovalResultMetadata(req),
			Input:      approvedInput,
		}, nil
	}
}

func isWorkspaceTargetTool(toolName string) bool {
	if toolName == "mcp_manage" || toolName == "app_manage" {
		return false
	}
	_, ok := toolapproval.OperationForTool(toolName)
	return ok
}

func ApprovalResultMetadata(req toolapproval.Request) map[string]any {
	metadata := map[string]any{
		"short_id":     req.ShortID,
		"status":       req.Status,
		"tool_name":    req.ToolName,
		"operation":    req.Operation,
		"tool_call_id": req.ToolCallID,
		// The nested approval payload is what the stream converter reads for
		// option-aware cards; without it the committed-decision publish path
		// drops the agent's options and the recorded selection.
		"approval": toolapproval.RequestMetadata(req),
	}
	if req.ExecutionLocation != nil {
		metadata[toolapproval.ExecutionLocationMetadataKey] = req.ExecutionLocation
	}
	return metadata
}

func executionLocationResultMetadata(location *toolapproval.ExecutionLocation) map[string]any {
	if location == nil {
		return nil
	}
	return map[string]any{toolapproval.ExecutionLocationMetadataKey: location}
}
