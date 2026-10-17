package application

import (
	"context"
	"errors"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/decide"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runconfig"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/agent/transcript"
	"github.com/felinics/memoh/internal/workspace"
)

func (s *Service) respondUserInput(ctx context.Context, input decide.UserInputResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := s.decider.CommitUserInputResponse(ctx, input)
	if err != nil {
		return err
	}
	return s.ContinueCommittedUserInputResponse(ctx, committed, eventCh)
}

// ContinueCommittedUserInputResponse resumes what a question parked when no
// session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (s *Service) ContinueCommittedUserInputResponse(ctx context.Context, committed decide.CommittedUserInputResponse, eventCh chan<- runreq.WSStreamEvent) error {
	resolved := committed.Request()
	if committed.AckOnly() {
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	if committed.IsExternalAgent() {
		// A waiter inside the running turn is blocked on this request and
		// resumes the run itself. When this response stream has reattached to the active ACP
		// prompt, forward that live continuation so refreshes observe the same
		// loading/progress shape as native deferred requests.
		if committed.ActivePrompt() != nil {
			return forwardExternalAgentActivePrompt(ctx, committed.ActivePrompt(), eventCh, externalAgentActivePromptForwardOptions{
				SkipToolCallID:  resolved.ToolCallID,
				SkipUserInputID: resolved.ID,
			})
		}
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	return s.driveUnroutedContinuation(ctx, s.userInputContinuation(ctx, committed, eventCh != nil), eventCh)
}

// userInputContinuation is the run an answered question resumes: the answer is
// stored as the ask_user call's result, and the model runs on from there. live
// says whether a caller receives the run's events as they happen.
func (s *Service) userInputContinuation(ctx context.Context, committed decide.CommittedUserInputResponse, live bool) decisionContinuation {
	resolved := decide.WithLocalWebUserInputReplyTarget(committed.Request())
	input := committed.Input()
	requestMessageID, reqErr := s.transcript.ContinuationRequestMessageID(ctx, resolved.SessionID, committed.RunHandle())
	req := runreq.ChatRequest{
		RunID:                   runreq.RunID(committed.RunID()),
		RunHandle:               committed.RunHandle(),
		BotID:                   input.BotID,
		ChatID:                  input.BotID,
		ThreadID:                resolved.SessionID,
		SourceChannelIdentityID: runreq.FirstNonEmpty(resolved.ChannelIdentityID, input.ActorChannelIdentityID),
		CurrentChannel:          resolved.SourcePlatform,
		ReplyTarget:             resolved.ReplyTarget,
		ConversationType:        resolved.ConversationType,
		UserMessagePersisted:    true,
		PersistedUserMessageID:  requestMessageID,
		// The answer is stored as the ask_user call's result, not as a new user
		// message, so neither write may schedule a user-message memory
		// extraction.
		SkipMemoryExtraction: true,
		WorkspaceTargetID:    resolved.WorkspaceTargetID,
	}
	return decisionContinuation{
		Req: req,
		// The resumed run is the driver's request, which carries the per-run
		// asset collector. Everything else in it is what this entry built.
		Setup: func(ctx context.Context, runReq runreq.ChatRequest) (drive.RunPlan, error) {
			if reqErr != nil {
				return drive.RunPlan{}, reqErr
			}
			if strings.TrimSpace(resolved.ID) == "" {
				return drive.RunPlan{}, errors.New("committed user input response is missing its request")
			}
			toolResult := sdk.ToolResultPart{
				ToolCallID: resolved.ToolCallID,
				ToolName:   resolved.ToolName,
				Result:     toolexec.OutputFromValue(s.decider.LimitToolResultValue(resolved.Result, resolved.ToolName)),
				IsError:    false,
			}
			if s.continueUserInputFn != nil {
				return s.continueUserInputFn(ctx, resolved, input, toolResult)
			}
			return s.storeUserInputResultAndContinue(ctx, resolved, input, toolResult, runReq, live)
		},
	}
}

func (s *Service) storeUserInputResultAndContinue(
	ctx context.Context,
	request userinput.Request,
	input decide.UserInputResponseInput,
	result sdk.ToolResultPart,
	req runreq.ChatRequest,
	live bool,
) (drive.RunPlan, error) {
	request = decide.WithLocalWebUserInputReplyTarget(request)
	ctx = workspace.WithWorkspaceTarget(ctx, request.WorkspaceTargetID)
	target, err := s.runconfig.ResolveWorkspaceTargetSnapshot(ctx, input.BotID, request.WorkspaceTargetID)
	if err != nil {
		return drive.RunPlan{}, err
	}
	modelMessages := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.ToolMessage(result)})
	storeReq := req
	storeReq.WorkspaceTarget = target
	if err := s.persist.StoreRound(ctx, storeReq, modelMessages, "", persist.RoundOptions{AllowPendingToolCalls: true}); err != nil {
		return drive.RunPlan{}, err
	}
	return s.continueUserInputSession(ctx, request, input, req, live)
}

// continueUserInputSession resolves the built-in agent run that resumes after
// the answer is stored.
func (s *Service) continueUserInputSession(
	ctx context.Context,
	request userinput.Request,
	input decide.UserInputResponseInput,
	req runreq.ChatRequest,
	live bool,
) (drive.RunPlan, error) {
	request = decide.WithLocalWebUserInputReplyTarget(request)
	ctx = workspace.WithWorkspaceTarget(ctx, request.WorkspaceTargetID)
	resolved, err := s.runconfig.ForSession(ctx,
		input.BotID,
		request.SessionID,
		runreq.FirstNonEmpty(request.ChannelIdentityID, input.ActorChannelIdentityID),
		request.SourcePlatform,
		request.ReplyTarget,
		request.ConversationType,
		input.ChatToken,
	)
	if err != nil {
		return drive.RunPlan{}, err
	}
	resolved.RunConfig.RunID = req.RunID

	cfg, err := s.prepareContinuationRunConfig(
		ctx,
		resolved.RunConfig,
		transcript.ScopeFallbackFromUserInputRequest(request),
		transcript.SummaryScope(runreq.FirstNonEmpty(request.BotID, input.BotID), "", request.SessionID, request.ConversationType, "", request.ReplyTarget),
		live,
	)
	if err != nil {
		return drive.RunPlan{}, err
	}
	req.RunID = cfg.RunID
	req.WorkspaceTarget = runconfig.WorkspaceTargetFromRunConfig(resolved.RunConfig)
	return s.nativeContinuationPlan(ctx, req, cfg, resolved.ModelID)
}
