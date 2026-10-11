package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextlimit "github.com/felinics/memoh/internal/agent/context/limit"
	"github.com/felinics/memoh/internal/agent/decision"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	agenttools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/bots"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/workspace"
)

type ToolApprovalResponseInput struct {
	ControlID              string
	BotID                  string
	ThreadID               string
	ActorChannelIdentityID string
	ActorUserID            string
	ApprovalID             string
	ExplicitID             string
	ReplyExternalMessageID string
	Decision               string
	// OptionID names the agent-provided permission option the decider picked;
	// empty means the plain binary decision.
	OptionID                   string
	Reason                     string
	ChatToken                  string
	SuppressActivePromptAttach bool
}

type CommittedToolApprovalResponse struct {
	request            toolapproval.Request
	input              ToolApprovalResponseInput
	runID              string
	runHandle          sessionruntime.RunHandle
	usesDecisionWaiter bool
	activePrompt       *externalAgentActivePromptSubscription
	ackOnly            bool
}

func (s *Service) respondToolApproval(ctx context.Context, input ToolApprovalResponseInput, eventCh chan<- WSStreamEvent) error {
	committed, err := s.CommitToolApprovalResponse(ctx, input)
	if err != nil {
		return err
	}
	return s.ContinueCommittedToolApprovalResponse(ctx, committed, eventCh)
}

func (s *Service) CommitToolApprovalResponse(ctx context.Context, input ToolApprovalResponseInput) (CommittedToolApprovalResponse, error) {
	if s.toolApproval == nil {
		return CommittedToolApprovalResponse{}, errors.New("tool approval service not configured")
	}
	target, err := s.toolApproval.ResolveTarget(ctx, toolapproval.ResolveInput{
		BotID:                  input.BotID,
		SessionID:              input.ThreadID,
		ExplicitID:             firstNonEmpty(input.ExplicitID, input.ApprovalID),
		ReplyExternalMessageID: input.ReplyExternalMessageID,
	})
	if err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	isExternalAgent, err := s.isExternalAgentToolApprovalSession(ctx, target.SessionID)
	if err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	// Management tools retain their prepared operation inside the active call.
	// A response wakes that waiter; replaying the tool would prepare and execute
	// a second operation instead of the exact change the user approved.
	usesDecisionWaiter := isExternalAgent || isCapabilityManagementApproval(target)
	ctx = workspace.WithWorkspaceTarget(ctx, target.WorkspaceTargetID)
	if isExternalAgent {
		if err := s.authorizeExternalAgentToolApprovalResponse(ctx, target, input); err != nil {
			return CommittedToolApprovalResponse{}, err
		}
	} else if err := s.authorizeToolApprovalResponse(ctx, target, input); err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	if usesDecisionWaiter && !s.toolApproval.CanRespond(target) {
		if _, err := s.toolApproval.Reject(ctx, target.ID, "", "tool approval expired: the requesting tool call is no longer waiting"); err != nil && !errors.Is(err, toolapproval.ErrAlreadyDecided) {
			return CommittedToolApprovalResponse{}, err
		}
		return CommittedToolApprovalResponse{request: target, input: input, usesDecisionWaiter: true, ackOnly: true}, nil
	}
	if !usesDecisionWaiter {
		ctx = decision.WithNativeContinuation(ctx)
	}
	decision := strings.ToLower(strings.TrimSpace(input.Decision))
	optionID := input.OptionID
	if optionID == "" && len(target.Options) > 0 {
		// Older Web/Desktop clients only send approve/reject. Preserve that
		// contract without inventing a durable choice: a binary response maps
		// only to the agent's one-shot option. Session/always options must be
		// selected explicitly by a client that can show their semantics.
		optionID, err = legacyPermissionOptionID(target.Options, decision)
	}
	if err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	var activePrompt *externalAgentActivePromptSubscription
	if isExternalAgent && !input.SuppressActivePromptAttach {
		activePrompt, _ = s.subscribeExternalAgentActivePrompt(
			firstNonEmpty(target.BotID, input.BotID),
			firstNonEmpty(target.SessionID, input.ThreadID),
		)
	}
	if optionID != "" {
		// The decider picked one of the agent's own options. Its kind is
		// authoritative for approve-vs-reject; a contradicting decision label
		// is an error rather than a silent reinterpretation.
		option, ok := toolapproval.FindOption(target.Options, optionID)
		switch {
		case !ok:
			err = fmt.Errorf("%w: unknown option %q", toolapproval.ErrOptionUnavailable, optionID)
		case option.Approves():
			if decision != "" && decision != "approve" && decision != "approved" {
				err = fmt.Errorf("%w: decision %q does not match allow option %q", toolapproval.ErrOptionUnavailable, input.Decision, optionID)
				break
			}
			target, err = s.toolApproval.ApproveOption(ctx, target.ID, input.ActorChannelIdentityID, input.Reason, optionID)
		default:
			if decision != "" && decision != "reject" && decision != "rejected" {
				err = fmt.Errorf("%w: decision %q does not match reject option %q", toolapproval.ErrOptionUnavailable, input.Decision, optionID)
				break
			}
			target, err = s.toolApproval.RejectOption(ctx, target.ID, input.ActorChannelIdentityID, input.Reason, optionID)
		}
	} else {
		switch decision {
		case "approve", "approved":
			target, err = s.toolApproval.Approve(ctx, target.ID, input.ActorChannelIdentityID, input.Reason)
		case "reject", "rejected":
			target, err = s.toolApproval.Reject(ctx, target.ID, input.ActorChannelIdentityID, input.Reason)
		default:
			err = fmt.Errorf("unknown tool approval decision %q", input.Decision)
		}
	}
	if err != nil {
		if activePrompt != nil {
			activePrompt.release()
		}
		return CommittedToolApprovalResponse{}, err
	}
	return CommittedToolApprovalResponse{
		request:            target,
		input:              input,
		usesDecisionWaiter: usesDecisionWaiter,
		activePrompt:       activePrompt,
	}, nil
}

func legacyPermissionOptionID(options []toolapproval.PermissionOption, decision string) (string, error) {
	wantKind := ""
	switch decision {
	case "approve", "approved":
		wantKind = toolapproval.OptionKindAllowOnce
	case "reject", "rejected":
		wantKind = toolapproval.OptionKindRejectOnce
	default:
		// Preserve the existing unknown-decision error below.
		return "", nil
	}
	match := ""
	matches := 0
	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(option.Kind), wantKind) {
			matches++
			if matches > 1 {
				match = ""
				break
			}
			match = option.ID
		}
	}
	if matches == 1 {
		return match, nil
	}
	if wantKind == toolapproval.OptionKindRejectOnce {
		// A binary rejection is always safe. With no reject_once option — or
		// an ambiguous set of several — the ACP adapter converts the rejected
		// result to a cancelled outcome instead of guessing an option or
		// selecting a broader reject_always scope.
		return "", nil
	}
	if matches > 1 {
		return "", fmt.Errorf("%w: legacy %s matches more than one %s option", toolapproval.ErrOptionUnavailable, decision, wantKind)
	}
	// No allow_once option exists, so a binary approve cannot be honored:
	// Memoh never persists ACP permission grants on the user's behalf, and
	// every remaining allow option carries always/session persistence the
	// binary surface could not show. The decider gets an explicit error and
	// must approve from a surface that renders the agent's options.
	return "", fmt.Errorf("%w: legacy %s requires an agent-provided %s option", toolapproval.ErrOptionUnavailable, decision, wantKind)
}

// ContinueCommittedToolApprovalResponse resumes what an approval parked when
// no session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (s *Service) ContinueCommittedToolApprovalResponse(ctx context.Context, committed CommittedToolApprovalResponse, eventCh chan<- WSStreamEvent) error {
	target := committed.request
	if committed.ackOnly {
		return emitApprovalAck(ctx, eventCh)
	}
	if committed.usesDecisionWaiter {
		if committed.activePrompt != nil {
			return forwardExternalAgentActivePrompt(ctx, committed.activePrompt, eventCh, externalAgentActivePromptForwardOptions{
				SkipToolCallID: target.ToolCallID,
				SkipApprovalID: target.ID,
			})
		}
		return emitApprovalAck(ctx, eventCh)
	}
	return s.driveUnroutedContinuation(ctx, s.toolApprovalContinuation(ctx, committed, eventCh != nil), eventCh)
}

// toolApprovalContinuation is the run an answered approval resumes: the
// decided call's result is stored as its answer, and the model runs on from
// there. live says whether a caller receives the run's events as they happen.
func (s *Service) toolApprovalContinuation(ctx context.Context, committed CommittedToolApprovalResponse, live bool) decisionContinuation {
	approval := withLocalWebReplyTarget(committed.request)
	input := committed.input
	requestMessageID, reqErr := s.continuationTurnRequestMessageID(ctx, approval.SessionID, committed.runHandle)
	req := ChatRequest{
		RunID:                   runIDForChatRequest(committed.runID),
		RunHandle:               committed.runHandle,
		BotID:                   input.BotID,
		ChatID:                  input.BotID,
		ThreadID:                approval.SessionID,
		SourceChannelIdentityID: firstNonEmpty(approval.ChannelIdentityID, input.ActorChannelIdentityID),
		CurrentChannel:          approval.SourcePlatform,
		ReplyTarget:             approval.ReplyTarget,
		ConversationType:        approval.ConversationType,
		UserMessagePersisted:    true,
		PersistedUserMessageID:  requestMessageID,
		WorkspaceTargetID:       approval.WorkspaceTargetID,
	}
	return decisionContinuation{
		Req: req,
		// The resumed run is the driver's request, which carries the per-run
		// asset collector. Everything else in it is what this entry built.
		Setup: func(ctx context.Context, runReq ChatRequest) (runPlan, error) {
			if reqErr != nil {
				return runPlan{}, reqErr
			}
			if strings.TrimSpace(approval.ID) == "" {
				return runPlan{}, errors.New("committed tool approval response is missing its request")
			}
			var toolResult sdk.ToolResultPart
			var toolUIMetadata map[string]any
			switch approval.Status {
			case toolapproval.StatusApproved:
				result, uiMetadata, err := s.executeApprovedTool(ctx, approval, input, runReq.RunID)
				if err != nil {
					return runPlan{}, err
				}
				toolResult = result
				toolUIMetadata = uiMetadata
			case toolapproval.StatusRejected:
				toolResult = sdk.ToolResultPart{
					ToolCallID: approval.ToolCallID,
					ToolName:   approval.ToolName,
					Result:     toolexec.OutputFromValue(s.limitToolResultText(rejectedToolResultText(input.Reason), approval.ToolName)),
					IsError:    true,
				}
			default:
				return runPlan{}, fmt.Errorf("committed tool approval has unexpected status %q", approval.Status)
			}
			return s.storeToolResultAndContinue(ctx, approval, input, toolResult, toolUIMetadata, runReq, live)
		},
	}
}

func (s *Service) toolOutputLimit() contextlimit.ToolOutputLimit {
	limit := native.DefaultLimits().ToolOutputLimit()
	if s != nil && s.agent != nil {
		limit = s.agent.Limits().ToolOutputLimit()
	}
	return limit
}

func (s *Service) limitToolResultText(text, toolName string) string {
	limit := s.toolOutputLimit()
	return contextlimit.LimitString(text, "tool result ("+toolName+")", limit)
}

func (s *Service) limitToolResultValue(value any, toolName string) any {
	return contextlimit.LimitToolOutput(value, "tool result ("+toolName+")", s.toolOutputLimit())
}

func (s *Service) limitToolApprovalResult(result toolexec.ToolApprovalResult, toolName string) toolexec.ToolApprovalResult {
	if result.Decision == toolexec.ToolApprovalDecisionRejected {
		result.Reason = s.limitToolResultText(result.Reason, toolName)
	}
	return result
}

func (s *Service) isExternalAgentToolApprovalSession(ctx context.Context, sessionID string) (bool, error) {
	if s == nil || s.sessionService == nil {
		return false, nil
	}
	sess, err := s.sessionService.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sessionpkg.UsesDecisionWaiter(sess), nil
}

func (s *Service) authorizeExternalAgentToolApprovalResponse(ctx context.Context, target toolapproval.Request, input ToolApprovalResponseInput) error {
	if s == nil || s.sessionService == nil {
		return errors.New("session service not configured")
	}
	sessionID := firstNonEmpty(target.SessionID, input.ThreadID)
	sess, err := s.sessionService.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if !sessionpkg.UsesDecisionWaiter(sess) {
		return s.authorizeToolApprovalResponse(ctx, target, input)
	}
	botID := firstNonEmpty(target.BotID, input.BotID)
	if strings.TrimSpace(sess.BotID) != "" && strings.TrimSpace(botID) != "" && sess.BotID != botID {
		return toolapproval.ErrForbidden
	}
	if strings.TrimSpace(botID) == "" {
		botID = sess.BotID
	}
	target.BotID = botID
	actorID := firstNonEmpty(input.ActorUserID, input.ActorChannelIdentityID)
	if actorID == "" {
		return toolapproval.ErrForbidden
	}
	runtimeOwnerID := metadataString(runtimeSessionMeta(sess), "runtime_owner_account_id")
	if runtimeOwnerID == "" {
		return toolapproval.ErrForbidden
	}
	// The runtime owner has no standing beyond their live grants: a revoked
	// or offboarded owner must lose approval authority at decision time, so
	// every actor — owner included — passes the permission check.
	return s.authorizeToolApprovalResponse(ctx, target, input)
}

func (s *Service) authorizeToolApprovalResponse(ctx context.Context, target toolapproval.Request, input ToolApprovalResponseInput) error {
	if s == nil || s.botPermissions == nil {
		return errors.New("bot permission checker not configured")
	}
	botID := firstNonEmpty(target.BotID, input.BotID)
	// Channel deciders without a bound account carry only their channel
	// identity; grants are keyed on that identity, so it stays a valid
	// authorization subject (base behavior).
	actorID := firstNonEmpty(input.ActorUserID, input.ActorChannelIdentityID)
	permission, ok := toolApprovalPermission(target.Operation)
	if isCapabilityManagementApproval(target) {
		permission, ok = bots.PermissionManage, true
	}
	if strings.TrimSpace(botID) == "" || strings.TrimSpace(actorID) == "" || !ok {
		return toolapproval.ErrForbidden
	}
	if ok, err := s.botPermissions.HasBotPermission(ctx, botID, actorID, permission); err != nil {
		return err
	} else if !ok {
		return toolapproval.ErrForbidden
	}
	return nil
}

func isCapabilityManagementApproval(target toolapproval.Request) bool {
	return target.ToolName == agenttools.ToolMCPManage().String() || target.ToolName == agenttools.ToolAppManage().String()
}

func toolApprovalPermission(operation string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case toolapproval.OperationRead:
		return bots.PermissionWorkspaceRead, true
	case toolapproval.OperationWrite:
		return bots.PermissionWorkspaceWrite, true
	case toolapproval.OperationExec:
		return bots.PermissionWorkspaceExec, true
	case toolapproval.OperationPermission:
		// A generic agent permission request (network grant, mode switch)
		// authorizes the agent to act, so it sits at the same authority as
		// running the agent's own commands.
		return bots.PermissionWorkspaceExec, true
	default:
		return "", false
	}
}

func emitApprovalAck(ctx context.Context, eventCh chan<- WSStreamEvent) error {
	if eventCh == nil {
		return nil
	}
	for _, event := range []native.StreamEvent{
		{Type: native.EventAgentStart},
		{Type: native.EventAgentEnd},
	} {
		if err := sendAgentStreamEvent(ctx, eventCh, event); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) executeApprovedTool(ctx context.Context, req toolapproval.Request, input ToolApprovalResponseInput, runID string) (sdk.ToolResultPart, map[string]any, error) {
	ctx = workspace.WithWorkspaceTarget(ctx, req.WorkspaceTargetID)
	req = withLocalWebReplyTarget(req)
	resolved, err := s.ResolveRunConfig(ctx,
		input.BotID,
		req.SessionID,
		firstNonEmpty(req.ChannelIdentityID, input.ActorChannelIdentityID),
		req.SourcePlatform,
		req.ReplyTarget,
		req.ConversationType,
		input.ChatToken,
	)
	if err != nil {
		return sdk.ToolResultPart{}, nil, err
	}
	resolved.RunConfig.RunID = runIDForChatRequest(runID)
	if fence, ok := runtimefence.FromContext(ctx); ok {
		var count int64
		err := runtimefence.InTransaction(ctx, s.queries, fence.BotID, fence.SessionID, func(queries dbstore.Queries) error {
			var err error
			count, err = queries.MarkSessionRunDecisionExecuting(ctx, sqlc.MarkSessionRunDecisionExecutingParams{
				BotID: db.ParseUUIDOrEmpty(fence.BotID), SessionID: db.ParseUUIDOrEmpty(fence.SessionID),
				RunID: db.ParseUUIDOrEmpty(runID), FencingToken: fence.Token, DecisionID: req.ID,
			})
			return err
		})
		if err != nil {
			return sdk.ToolResultPart{}, nil, fmt.Errorf("checkpoint approved tool execution: %w", err)
		}
		if count != 1 {
			return sdk.ToolResultPart{}, nil, sessionruntime.ErrRunOwnershipLost
		}
	}
	part, uiMetadata, err := s.agent.ExecuteToolWithUIMetadata(ctx, resolved.RunConfig, sdk.ToolCall{
		ToolCallID: req.ToolCallID,
		ToolName:   req.ToolName,
		Input:      toolexec.ArgumentsFromValue(req.ToolInput),
	})
	return part, uiMetadata, err
}

func (s *Service) storeToolResultAndContinue(
	ctx context.Context,
	approval toolapproval.Request,
	input ToolApprovalResponseInput,
	result sdk.ToolResultPart,
	uiMetadata map[string]any,
	req ChatRequest,
	live bool,
) (runPlan, error) {
	approval = withLocalWebReplyTarget(approval)
	ctx = workspace.WithWorkspaceTarget(ctx, approval.WorkspaceTargetID)
	target, err := s.resolveWorkspaceTargetSnapshot(ctx, input.BotID, approval.WorkspaceTargetID)
	if err != nil {
		return runPlan{}, err
	}
	modelMessages := sdkMessagesToModelMessages([]sdk.Message{sdk.ToolMessage(result)})
	storeOpts := storeRoundOptions{AllowPendingToolCalls: true}
	// UI-only payloads stripped from the tool output (e.g. the edit diff) ride
	// the tool row's metadata — the model history reads only the content.
	if len(uiMetadata) > 0 {
		storeOpts.MessageMetadataByIndex = map[int]map[string]any{0: uiMetadata}
	}
	storeReq := req
	storeReq.WorkspaceTarget = target
	if err := s.storeRoundWithOptions(ctx, storeReq, modelMessages, "", storeOpts); err != nil {
		return runPlan{}, err
	}
	return s.continueToolApprovalSession(ctx, approval, input, req, live)
}

// continueToolApprovalSession resolves the built-in agent run that resumes
// after the approved or rejected call's result is stored.
func (s *Service) continueToolApprovalSession(
	ctx context.Context,
	approval toolapproval.Request,
	input ToolApprovalResponseInput,
	req ChatRequest,
	live bool,
) (runPlan, error) {
	approval = withLocalWebReplyTarget(approval)
	ctx = workspace.WithWorkspaceTarget(ctx, approval.WorkspaceTargetID)
	resolved, err := s.ResolveRunConfig(ctx,
		input.BotID,
		approval.SessionID,
		firstNonEmpty(approval.ChannelIdentityID, input.ActorChannelIdentityID),
		approval.SourcePlatform,
		approval.ReplyTarget,
		approval.ConversationType,
		input.ChatToken,
	)
	if err != nil {
		return runPlan{}, err
	}
	resolved.RunConfig.RunID = req.RunID

	cfg, err := s.prepareContinuationRunConfig(
		ctx,
		resolved.RunConfig,
		historyScopeFallbackFromToolApprovalRequest(approval),
		compactionSummaryScope(firstNonEmpty(approval.BotID, input.BotID), "", approval.SessionID, approval.ConversationType, "", approval.ReplyTarget),
		live,
	)
	if err != nil {
		return runPlan{}, err
	}
	req.RunID = cfg.RunID
	req.WorkspaceTarget = workspaceTargetFromRunConfig(resolved.RunConfig)
	return s.nativeContinuationPlan(ctx, req, cfg, resolved.ModelID)
}

func withLocalWebReplyTarget(req toolapproval.Request) toolapproval.Request {
	if strings.EqualFold(strings.TrimSpace(req.SourcePlatform), "web") && strings.TrimSpace(req.ReplyTarget) == "" {
		req.ReplyTarget = strings.TrimSpace(req.BotID)
	}
	return req
}

func rejectedToolResultText(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "tool execution rejected by user"
	}
	return "tool execution rejected by user: " + reason
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
