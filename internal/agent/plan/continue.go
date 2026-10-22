package plan

import (
	"context"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/workspace"
)

// Continue plans the run an answered decision resumes: the decided call is run
// when the decision approved it, its result is stored as the answer to the call
// the run parked on, and the model runs on from there. req is the request the
// driver handed the run, and live says whether a caller receives the run's
// events as they happen. The call has passed decide.DecidedCall.Validate.
func (p *Planner) Continue(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error) {
	if call.RunID == "" {
		// An answer no session runtime routed has no admitted run, hence no
		// fence and no step committer to write the resumed history with. The
		// decision predates the run ledger, so the request can no longer
		// continue: the approved call is not run and nothing is written.
		return drive.RunPlan{}, expiredDecision(call.Kind)
	}
	ctx = workspace.WithWorkspaceTarget(ctx, call.WorkspaceTargetID)

	result := call.Result
	var uiMetadata map[string]any
	if call.Verdict == decide.VerdictApproved {
		var err error
		if result, uiMetadata, err = p.executeApproved(ctx, call, req.RunID); err != nil {
			return drive.RunPlan{}, err
		}
	}
	if err := p.storeResult(ctx, call, result, uiMetadata, req); err != nil {
		return drive.RunPlan{}, err
	}
	return p.resume(ctx, call, req, live)
}

// executeApproved runs the call a decision approved, inside the run that
// resumes.
func (p *Planner) executeApproved(ctx context.Context, call decide.DecidedCall, runID string) (sdk.ToolResultPart, map[string]any, error) {
	resolved, err := p.session(ctx, call)
	if err != nil {
		return sdk.ToolResultPart{}, nil, err
	}
	resolved.RunConfig.RunID = runreq.RunID(runID)
	if err := p.executions.MarkExecuting(ctx, runID, call.DecisionID); err != nil {
		return sdk.ToolResultPart{}, nil, err
	}
	return p.agent.ExecuteToolWithUIMetadata(ctx, resolved.RunConfig, sdk.ToolCall{
		ToolCallID: call.ToolCallID,
		ToolName:   call.ToolName,
		Input:      toolexec.ArgumentsFromValue(call.ToolInput),
	})
}

// storeResult writes the call's result into the session as the answer to the
// call the run parked on.
func (p *Planner) storeResult(ctx context.Context, call decide.DecidedCall, result sdk.ToolResultPart, uiMetadata map[string]any, req runreq.ChatRequest) error {
	target, err := p.runconfig.ResolveWorkspaceTargetSnapshot(ctx, call.BotID, call.WorkspaceTargetID)
	if err != nil {
		return err
	}
	modelMessages := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.ToolMessage(result)})
	storeOpts := persist.RoundOptions{AllowPendingToolCalls: true}
	// UI-only payloads stripped from the tool output (e.g. the edit diff) ride
	// the tool row's metadata; the model history reads only the content.
	if len(uiMetadata) > 0 {
		storeOpts.MessageMetadataByIndex = map[int]map[string]any{0: uiMetadata}
	}
	storeReq := req
	storeReq.WorkspaceTarget = target
	return p.persist.StoreRound(ctx, storeReq, modelMessages, "", storeOpts)
}

// resume plans the native run that continues from the session once the
// call's result is stored. Its steps commit through the live queue the run was
// admitted with, so the resumed steps continue the parked run's numbering.
func (p *Planner) resume(ctx context.Context, call decide.DecidedCall, req runreq.ChatRequest, live bool) (drive.RunPlan, error) {
	resolved, err := p.session(ctx, call)
	if err != nil {
		return drive.RunPlan{}, err
	}
	resolved.RunConfig.RunID = req.RunID

	cfg, err := p.assembleContinuation(
		ctx,
		resolved.RunConfig,
		transcript.SummaryScope(call.ScopeBotID, "", call.SessionID, call.ConversationType, "", call.ReplyTarget),
	)
	if err != nil {
		return drive.RunPlan{}, err
	}
	req.RunID = cfg.RunID
	req.WorkspaceTarget = runconfig.WorkspaceTargetFromRunConfig(resolved.RunConfig)
	rc := runreq.ResolvedContext{RunConfig: cfg, Model: models.GetResponse{ID: resolved.ModelID}}
	// A question asked with nobody receiving the run's events could never be
	// answered, so asking one is only offered to a run someone follows live.
	return p.nativePlan(ctx, req, rc, cfg, nativeOptions{
		LiveToolStream:      live,
		CanRequestUserInput: live,
		Steps:               stepsResume,
	})
}

// session is the run configuration of the session the decision parked in.
func (p *Planner) session(ctx context.Context, call decide.DecidedCall) (runconfig.SessionRunConfig, error) {
	return p.runconfig.ForSession(ctx,
		call.BotID,
		call.SessionID,
		call.ChannelIdentityID,
		call.SourcePlatform,
		call.ReplyTarget,
		call.ConversationType,
		call.ChatToken,
	)
}

// expiredDecision is the failure of an answer whose decision can no longer
// continue.
func expiredDecision(kind decide.CallKind) error {
	if kind == decide.CallUserInput {
		return apperror.New(apperror.CodeUserInputExpired, nil)
	}
	return apperror.New(apperror.CodeToolApprovalExpired, nil)
}
