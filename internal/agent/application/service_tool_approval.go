package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/decide"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/workspace"
)

func (s *Service) respondToolApproval(ctx context.Context, input decide.ToolApprovalResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := s.decider.CommitToolApprovalResponse(ctx, input)
	if err != nil {
		return err
	}
	return s.ContinueCommittedToolApprovalResponse(ctx, committed, eventCh)
}

// ContinueCommittedToolApprovalResponse resumes what an approval parked when
// no session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (s *Service) ContinueCommittedToolApprovalResponse(ctx context.Context, committed decide.CommittedToolApprovalResponse, eventCh chan<- runreq.WSStreamEvent) error {
	target := committed.Request()
	if committed.AckOnly() {
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	if committed.UsesDecisionWaiter() {
		if committed.ActivePrompt() != nil {
			return forwardExternalAgentActivePrompt(ctx, committed.ActivePrompt(), eventCh, externalAgentActivePromptForwardOptions{
				SkipToolCallID: target.ToolCallID,
				SkipApprovalID: target.ID,
			})
		}
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	return s.driveUnroutedContinuation(ctx, s.toolApprovalContinuation(ctx, committed, eventCh != nil), eventCh)
}

// toolApprovalContinuation is the run an answered approval resumes: the
// decided call's result is stored as its answer, and the model runs on from
// there. live says whether a caller receives the run's events as they happen.
func (s *Service) toolApprovalContinuation(ctx context.Context, committed decide.CommittedToolApprovalResponse, live bool) decisionContinuation {
	approval := decide.WithLocalWebReplyTarget(committed.Request())
	input := committed.Input()
	requestMessageID, reqErr := s.transcript.ContinuationRequestMessageID(ctx, approval.SessionID, committed.RunHandle())
	req := runreq.ChatRequest{
		RunID:                   runreq.RunID(committed.RunID()),
		RunHandle:               committed.RunHandle(),
		BotID:                   input.BotID,
		ChatID:                  input.BotID,
		ThreadID:                approval.SessionID,
		SourceChannelIdentityID: runreq.FirstNonEmpty(approval.ChannelIdentityID, input.ActorChannelIdentityID),
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
		Setup: func(ctx context.Context, runReq runreq.ChatRequest) (drive.RunPlan, error) {
			if reqErr != nil {
				return drive.RunPlan{}, reqErr
			}
			if strings.TrimSpace(approval.ID) == "" {
				return drive.RunPlan{}, errors.New("committed tool approval response is missing its request")
			}
			var toolResult sdk.ToolResultPart
			var toolUIMetadata map[string]any
			switch approval.Status {
			case toolapproval.StatusApproved:
				result, uiMetadata, err := s.executeApprovedTool(ctx, approval, input, runReq.RunID)
				if err != nil {
					return drive.RunPlan{}, err
				}
				toolResult = result
				toolUIMetadata = uiMetadata
			case toolapproval.StatusRejected:
				toolResult = sdk.ToolResultPart{
					ToolCallID: approval.ToolCallID,
					ToolName:   approval.ToolName,
					Result:     toolexec.OutputFromValue(s.decider.LimitToolResultText(decide.RejectedToolResultText(input.Reason), approval.ToolName)),
					IsError:    true,
				}
			default:
				return drive.RunPlan{}, fmt.Errorf("committed tool approval has unexpected status %q", approval.Status)
			}
			return s.storeToolResultAndContinue(ctx, approval, input, toolResult, toolUIMetadata, runReq, live)
		},
	}
}

func (s *Service) executeApprovedTool(ctx context.Context, req toolapproval.Request, input decide.ToolApprovalResponseInput, runID string) (sdk.ToolResultPart, map[string]any, error) {
	ctx = workspace.WithWorkspaceTarget(ctx, req.WorkspaceTargetID)
	req = decide.WithLocalWebReplyTarget(req)
	resolved, err := s.runconfig.ForSession(ctx,
		input.BotID,
		req.SessionID,
		runreq.FirstNonEmpty(req.ChannelIdentityID, input.ActorChannelIdentityID),
		req.SourcePlatform,
		req.ReplyTarget,
		req.ConversationType,
		input.ChatToken,
	)
	if err != nil {
		return sdk.ToolResultPart{}, nil, err
	}
	resolved.RunConfig.RunID = runreq.RunID(runID)
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
	input decide.ToolApprovalResponseInput,
	result sdk.ToolResultPart,
	uiMetadata map[string]any,
	req runreq.ChatRequest,
	live bool,
) (drive.RunPlan, error) {
	approval = decide.WithLocalWebReplyTarget(approval)
	ctx = workspace.WithWorkspaceTarget(ctx, approval.WorkspaceTargetID)
	target, err := s.runconfig.ResolveWorkspaceTargetSnapshot(ctx, input.BotID, approval.WorkspaceTargetID)
	if err != nil {
		return drive.RunPlan{}, err
	}
	modelMessages := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.ToolMessage(result)})
	storeOpts := persist.RoundOptions{AllowPendingToolCalls: true}
	// UI-only payloads stripped from the tool output (e.g. the edit diff) ride
	// the tool row's metadata — the model history reads only the content.
	if len(uiMetadata) > 0 {
		storeOpts.MessageMetadataByIndex = map[int]map[string]any{0: uiMetadata}
	}
	storeReq := req
	storeReq.WorkspaceTarget = target
	if err := s.persist.StoreRound(ctx, storeReq, modelMessages, "", storeOpts); err != nil {
		return drive.RunPlan{}, err
	}
	return s.continueToolApprovalSession(ctx, approval, input, req, live)
}

// continueToolApprovalSession resolves the built-in agent run that resumes
// after the approved or rejected call's result is stored.
func (s *Service) continueToolApprovalSession(
	ctx context.Context,
	approval toolapproval.Request,
	input decide.ToolApprovalResponseInput,
	req runreq.ChatRequest,
	live bool,
) (drive.RunPlan, error) {
	approval = decide.WithLocalWebReplyTarget(approval)
	ctx = workspace.WithWorkspaceTarget(ctx, approval.WorkspaceTargetID)
	resolved, err := s.runconfig.ForSession(ctx,
		input.BotID,
		approval.SessionID,
		runreq.FirstNonEmpty(approval.ChannelIdentityID, input.ActorChannelIdentityID),
		approval.SourcePlatform,
		approval.ReplyTarget,
		approval.ConversationType,
		input.ChatToken,
	)
	if err != nil {
		return drive.RunPlan{}, err
	}
	resolved.RunConfig.RunID = req.RunID

	cfg, err := s.prepareContinuationRunConfig(
		ctx,
		resolved.RunConfig,
		transcript.ScopeFallbackFromToolApprovalRequest(approval),
		transcript.SummaryScope(runreq.FirstNonEmpty(approval.BotID, input.BotID), "", approval.SessionID, approval.ConversationType, "", approval.ReplyTarget),
		live,
	)
	if err != nil {
		return drive.RunPlan{}, err
	}
	req.RunID = cfg.RunID
	req.WorkspaceTarget = runconfig.WorkspaceTargetFromRunConfig(resolved.RunConfig)
	return s.nativeContinuationPlan(ctx, req, cfg, resolved.ModelID)
}

// CanDecideToolApproval is the single authorization for answering a tool
// approval; see decide.Decider.CanDecideToolApproval.
func (s *Service) CanDecideToolApproval(ctx context.Context, approval toolapproval.Request, principal decide.ToolApprovalPrincipal) (bool, error) {
	return s.decider.CanDecideToolApproval(ctx, approval, principal)
}

// ViewerCanDecideToolApproval answers CanDecideToolApproval for an approval a
// live frame names only by id.
func (s *Service) ViewerCanDecideToolApproval(ctx context.Context, approvalID string, principal decide.ToolApprovalPrincipal) (bool, error) {
	return s.decider.ViewerCanDecideToolApproval(ctx, approvalID, principal)
}
