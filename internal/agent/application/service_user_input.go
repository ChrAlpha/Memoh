package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/decision"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/toolexec"
	"github.com/felinics/memoh/internal/bots"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/i18n"
	"github.com/felinics/memoh/internal/workspace"
)

// userInputService is the slice of *userinput.Service the application depends
// on, kept as an interface so the respond/resume routing can be tested with
// fakes.
type userInputService interface {
	Get(ctx context.Context, requestID string) (userinput.Request, error)
	CreatePending(ctx context.Context, input userinput.CreatePendingInput) (userinput.Request, error)
	ResolveTarget(ctx context.Context, input userinput.ResolveInput) (userinput.Request, error)
	AdvanceText(ctx context.Context, input userinput.AdvanceTextInput) (userinput.AdvanceTextResult, error)
	Submit(ctx context.Context, input userinput.SubmitInput) (userinput.Request, error)
	Cancel(ctx context.Context, input userinput.CancelInput) (userinput.Request, error)
	CanRespond(req userinput.Request) bool
}

func (s *Service) AdvancePlainTextUserInput(ctx context.Context, input userinput.AdvanceTextInput) (userinput.AdvanceTextResult, error) {
	if s.userInput == nil {
		return userinput.AdvanceTextResult{}, errors.New("user input service not configured")
	}
	return s.userInput.AdvanceText(ctx, input)
}

type UserInputResponseInput struct {
	ControlID                  string
	BotID                      string
	ThreadID                   string
	ActorChannelIdentityID     string
	ActorUserID                string
	UserInputID                string
	ExplicitID                 string
	ReplyExternalMessageID     string
	Answers                    []userinput.QuestionAnswer
	UILanguage                 string
	TextAnswer                 string
	Canceled                   bool
	Reason                     string
	ChatToken                  string
	SuppressActivePromptAttach bool
}

func (s *Service) respondUserInput(ctx context.Context, input UserInputResponseInput, eventCh chan<- runreq.WSStreamEvent) error {
	committed, err := s.CommitUserInputResponse(ctx, input)
	if err != nil {
		return err
	}
	return s.ContinueCommittedUserInputResponse(ctx, committed, eventCh)
}

// CommittedUserInputResponse is the durable half of an ask_user response.
// Keeping it separate from the continuation lets the runtime acknowledge the
// user's click as soon as the decision commits, without imposing the command
// acknowledgement deadline on the following model call.
type CommittedUserInputResponse struct {
	request         userinput.Request
	input           UserInputResponseInput
	runID           string
	runHandle       sessionruntime.RunHandle
	activePrompt    *externalAgentActivePromptSubscription
	isExternalAgent bool
	ackOnly         bool
}

func (s *Service) CommitUserInputResponse(ctx context.Context, input UserInputResponseInput) (CommittedUserInputResponse, error) {
	if s.userInput == nil {
		return CommittedUserInputResponse{}, errors.New("user input service not configured")
	}
	target, err := s.userInput.ResolveTarget(ctx, userinput.ResolveInput{
		BotID:                  input.BotID,
		SessionID:              input.ThreadID,
		ExplicitID:             runreq.FirstNonEmpty(input.ExplicitID, input.UserInputID),
		ReplyExternalMessageID: input.ReplyExternalMessageID,
	})
	if err != nil {
		return CommittedUserInputResponse{}, err
	}

	isProcessLocalExternalAgent, err := s.isExternalAgentUserInputSession(ctx, runreq.FirstNonEmpty(target.SessionID, input.ThreadID))
	if err != nil {
		return CommittedUserInputResponse{}, err
	}
	if isProcessLocalExternalAgent {
		if err := s.authorizeExternalAgentUserInputResponse(ctx, target, input); err != nil {
			return CommittedUserInputResponse{}, err
		}
	}
	if !isProcessLocalExternalAgent {
		ctx = workspace.WithWorkspaceTarget(ctx, target.WorkspaceTargetID)
	}
	if isProcessLocalExternalAgent && !s.userInput.CanRespond(target) {
		if _, err := s.userInput.Cancel(ctx, userinput.CancelInput{
			RequestID:              target.ID,
			ActorChannelIdentityID: input.ActorChannelIdentityID,
			Reason:                 "user input expired: the requesting tool call is no longer waiting",
		}); err != nil && !errors.Is(err, userinput.ErrAlreadyDecided) {
			return CommittedUserInputResponse{}, err
		}
		return CommittedUserInputResponse{request: target, input: input, isExternalAgent: true, ackOnly: true}, nil
	}
	if !isProcessLocalExternalAgent {
		ctx = decision.WithNativeContinuation(ctx)
	}
	var activePrompt *externalAgentActivePromptSubscription
	if isProcessLocalExternalAgent && !input.SuppressActivePromptAttach {
		activePrompt, _ = s.subscribeExternalAgentActivePrompt(
			runreq.FirstNonEmpty(target.BotID, input.BotID),
			runreq.FirstNonEmpty(target.SessionID, input.ThreadID),
		)
	}

	var resolved userinput.Request
	if input.Canceled {
		resolved, err = s.userInput.Cancel(ctx, userinput.CancelInput{
			RequestID:              target.ID,
			ActorChannelIdentityID: input.ActorChannelIdentityID,
			Reason:                 input.Reason,
		})
	} else {
		answers := input.Answers
		if len(answers) == 0 && strings.TrimSpace(input.TextAnswer) != "" {
			answers, err = userInputAnswersFromText(target.UIPayload.Localized(i18n.New(input.UILanguage)), input.TextAnswer)
			if err != nil {
				if activePrompt != nil {
					activePrompt.release()
				}
				return CommittedUserInputResponse{}, err
			}
		}
		resolved, err = s.userInput.Submit(ctx, userinput.SubmitInput{
			RequestID:              target.ID,
			ActorChannelIdentityID: input.ActorChannelIdentityID,
			Answers:                answers,
		})
	}
	if err != nil {
		if activePrompt != nil {
			activePrompt.release()
		}
		if isProcessLocalExternalAgent && errors.Is(err, userinput.ErrAlreadyDecided) {
			return CommittedUserInputResponse{request: target, input: input, isExternalAgent: true, ackOnly: true}, nil
		}
		return CommittedUserInputResponse{}, err
	}
	return CommittedUserInputResponse{
		request:         resolved,
		input:           input,
		activePrompt:    activePrompt,
		isExternalAgent: isProcessLocalExternalAgent,
	}, nil
}

// ContinueCommittedUserInputResponse resumes what a question parked when no
// session runtime routed the answer: an inline runtime's waiter, or a
// built-in agent run driven here with eventCh as the caller's end of it.
func (s *Service) ContinueCommittedUserInputResponse(ctx context.Context, committed CommittedUserInputResponse, eventCh chan<- runreq.WSStreamEvent) error {
	resolved := committed.request
	if committed.ackOnly {
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	if committed.isExternalAgent {
		// A waiter inside the running turn is blocked on this request and
		// resumes the run itself. When this response stream has reattached to the active ACP
		// prompt, forward that live continuation so refreshes observe the same
		// loading/progress shape as native deferred requests.
		if committed.activePrompt != nil {
			return forwardExternalAgentActivePrompt(ctx, committed.activePrompt, eventCh, externalAgentActivePromptForwardOptions{
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
func (s *Service) userInputContinuation(ctx context.Context, committed CommittedUserInputResponse, live bool) decisionContinuation {
	resolved := withLocalWebUserInputReplyTarget(committed.request)
	input := committed.input
	requestMessageID, reqErr := s.continuationTurnRequestMessageID(ctx, resolved.SessionID, committed.runHandle)
	req := runreq.ChatRequest{
		RunID:                   runreq.RunID(committed.runID),
		RunHandle:               committed.runHandle,
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
		Setup: func(ctx context.Context, runReq runreq.ChatRequest) (runPlan, error) {
			if reqErr != nil {
				return runPlan{}, reqErr
			}
			if strings.TrimSpace(resolved.ID) == "" {
				return runPlan{}, errors.New("committed user input response is missing its request")
			}
			toolResult := sdk.ToolResultPart{
				ToolCallID: resolved.ToolCallID,
				ToolName:   resolved.ToolName,
				Result:     toolexec.OutputFromValue(s.limitToolResultValue(resolved.Result, resolved.ToolName)),
				IsError:    false,
			}
			if s.continueUserInputFn != nil {
				return s.continueUserInputFn(ctx, resolved, input, toolResult)
			}
			return s.storeUserInputResultAndContinue(ctx, resolved, input, toolResult, runReq, live)
		},
	}
}

// isExternalAgentUserInputSession classifies a request by its session's runtime, the
// same way isExternalAgentToolApprovalSession does for approvals: a decision-waiter
// runtime consumes the answer inside the blocked turn, so it must not become
// a native model continuation.
func (s *Service) isExternalAgentUserInputSession(ctx context.Context, sessionID string) (bool, error) {
	if s == nil || s.sessionService == nil {
		return false, nil
	}
	sess, err := s.sessionService.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sessionpkg.UsesDecisionWaiter(sess), nil
}

func (s *Service) authorizeExternalAgentUserInputResponse(ctx context.Context, target userinput.Request, input UserInputResponseInput) error {
	if s == nil || s.sessionService == nil {
		return errors.New("session service not configured")
	}
	sessionID := runreq.FirstNonEmpty(target.SessionID, input.ThreadID)
	sess, err := s.sessionService.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if !sessionpkg.UsesDecisionWaiter(sess) {
		return nil
	}
	botID := runreq.FirstNonEmpty(target.BotID, input.BotID)
	if strings.TrimSpace(sess.BotID) != "" && strings.TrimSpace(botID) != "" && sess.BotID != botID {
		return userinput.ErrForbidden
	}
	if strings.TrimSpace(botID) == "" {
		botID = sess.BotID
	}
	// Channel responders without a bound account carry only their channel
	// identity; grants are keyed on that identity, so it stays a valid
	// authorization subject (base behavior).
	actorID := runreq.FirstNonEmpty(input.ActorUserID, input.ActorChannelIdentityID)
	if actorID == "" {
		return userinput.ErrForbidden
	}
	runtimeOwnerID := runreq.MetadataString(runreq.RuntimeSessionMeta(sess), "runtime_owner_account_id")
	if runtimeOwnerID == "" {
		return userinput.ErrForbidden
	}
	// The runtime owner has no standing beyond their live grants: a revoked
	// or offboarded owner must lose response authority at decision time, so
	// every actor — owner included — passes the permission check.
	if s.botPermissions == nil {
		return errors.New("bot permission checker not configured")
	}
	if ok, err := s.botPermissions.HasBotPermission(ctx, botID, actorID, bots.PermissionWorkspaceExec); err != nil {
		return err
	} else if !ok {
		return userinput.ErrForbidden
	}
	return nil
}

func userInputAnswersFromText(payload userinput.UIPayload, text string) ([]userinput.QuestionAnswer, error) {
	answerText := strings.TrimSpace(text)
	if answerText == "" {
		return nil, errors.New("user input answer is required")
	}
	if len(payload.Questions) != 1 {
		return nil, errors.New("text response command can answer exactly one user input question")
	}
	question := payload.Questions[0]
	answer := userinput.QuestionAnswer{QuestionID: question.ID}
	switch question.Kind {
	case userinput.QuestionKindText:
		answer.Text = answerText
	case userinput.QuestionKindSingleSelect:
		optionID, ok := matchUserInputOption(question, answerText)
		switch {
		case ok:
			answer.OptionIDs = []string{optionID}
		case question.AllowCustom:
			answer.CustomText = answerText
		default:
			return nil, fmt.Errorf("answer %q does not match an option for question %q", answerText, question.ID)
		}
	case userinput.QuestionKindMultiSelect:
		parts := splitUserInputAnswerText(answerText)
		optionIDs := make([]string, 0, len(parts))
		custom := ""
		for _, part := range parts {
			if optionID, ok := matchUserInputOption(question, part); ok {
				optionIDs = append(optionIDs, optionID)
				continue
			}
			if question.AllowCustom && custom == "" {
				custom = part
				continue
			}
			return nil, fmt.Errorf("answer %q does not match an option for question %q", part, question.ID)
		}
		answer.OptionIDs = optionIDs
		answer.CustomText = custom
	default:
		return nil, fmt.Errorf("question %q has unsupported kind %q", question.ID, question.Kind)
	}
	return []userinput.QuestionAnswer{answer}, nil
}

func matchUserInputOption(question userinput.UIQuestion, text string) (string, bool) {
	target := strings.TrimSpace(text)
	for _, option := range question.Options {
		if strings.EqualFold(strings.TrimSpace(option.ID), target) || strings.EqualFold(strings.TrimSpace(option.Label), target) {
			return option.ID, true
		}
	}
	return "", false
}

func splitUserInputAnswerText(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；'
	})
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		if part := strings.TrimSpace(field); part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 && strings.TrimSpace(text) != "" {
		parts = append(parts, strings.TrimSpace(text))
	}
	return parts
}

func (s *Service) storeUserInputResultAndContinue(
	ctx context.Context,
	request userinput.Request,
	input UserInputResponseInput,
	result sdk.ToolResultPart,
	req runreq.ChatRequest,
	live bool,
) (runPlan, error) {
	request = withLocalWebUserInputReplyTarget(request)
	ctx = workspace.WithWorkspaceTarget(ctx, request.WorkspaceTargetID)
	target, err := s.resolveWorkspaceTargetSnapshot(ctx, input.BotID, request.WorkspaceTargetID)
	if err != nil {
		return runPlan{}, err
	}
	modelMessages := runreq.SDKMessagesToModelMessages([]sdk.Message{sdk.ToolMessage(result)})
	storeReq := req
	storeReq.WorkspaceTarget = target
	if err := s.storeRoundWithOptions(ctx, storeReq, modelMessages, "", storeRoundOptions{AllowPendingToolCalls: true}); err != nil {
		return runPlan{}, err
	}
	return s.continueUserInputSession(ctx, request, input, req, live)
}

// continueUserInputSession resolves the built-in agent run that resumes after
// the answer is stored.
func (s *Service) continueUserInputSession(
	ctx context.Context,
	request userinput.Request,
	input UserInputResponseInput,
	req runreq.ChatRequest,
	live bool,
) (runPlan, error) {
	request = withLocalWebUserInputReplyTarget(request)
	ctx = workspace.WithWorkspaceTarget(ctx, request.WorkspaceTargetID)
	resolved, err := s.ResolveRunConfig(ctx,
		input.BotID,
		request.SessionID,
		runreq.FirstNonEmpty(request.ChannelIdentityID, input.ActorChannelIdentityID),
		request.SourcePlatform,
		request.ReplyTarget,
		request.ConversationType,
		input.ChatToken,
	)
	if err != nil {
		return runPlan{}, err
	}
	resolved.RunConfig.RunID = req.RunID

	cfg, err := s.prepareContinuationRunConfig(
		ctx,
		resolved.RunConfig,
		historyScopeFallbackFromUserInputRequest(request),
		compactionSummaryScope(runreq.FirstNonEmpty(request.BotID, input.BotID), "", request.SessionID, request.ConversationType, "", request.ReplyTarget),
		live,
	)
	if err != nil {
		return runPlan{}, err
	}
	req.RunID = cfg.RunID
	req.WorkspaceTarget = workspaceTargetFromRunConfig(resolved.RunConfig)
	return s.nativeContinuationPlan(ctx, req, cfg, resolved.ModelID)
}

func withLocalWebUserInputReplyTarget(req userinput.Request) userinput.Request {
	if strings.EqualFold(strings.TrimSpace(req.SourcePlatform), "web") && strings.TrimSpace(req.ReplyTarget) == "" {
		req.ReplyTarget = strings.TrimSpace(req.BotID)
	}
	return req
}
