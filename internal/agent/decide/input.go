package decide

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/activeprompt"
	"github.com/felinics/memoh/internal/agent/decision"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/bots"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/i18n"
	"github.com/felinics/memoh/internal/workspace"
)

// userInputService is the slice of *userinput.Service the application depends
// on, kept as an interface so the respond/resume routing can be tested with
// fakes.
type UserInputs interface {
	Get(ctx context.Context, requestID string) (userinput.Request, error)
	CreatePending(ctx context.Context, input userinput.CreatePendingInput) (userinput.Request, error)
	ResolveTarget(ctx context.Context, input userinput.ResolveInput) (userinput.Request, error)
	AdvanceText(ctx context.Context, input userinput.AdvanceTextInput) (userinput.AdvanceTextResult, error)
	Submit(ctx context.Context, input userinput.SubmitInput) (userinput.Request, error)
	Cancel(ctx context.Context, input userinput.CancelInput) (userinput.Request, error)
	CanRespond(req userinput.Request) bool
}

func (d *Decider) AdvancePlainTextUserInput(ctx context.Context, input userinput.AdvanceTextInput) (userinput.AdvanceTextResult, error) {
	return d.userInput.AdvanceText(ctx, input)
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

// NewCommittedUserInput is the committed response of a question answered
// elsewhere: request as it stands after the answer, input as the responder sent
// it.
func NewCommittedUserInput(request userinput.Request, input UserInputResponseInput) CommittedUserInputResponse {
	return CommittedUserInputResponse{request: request, input: input}
}

func (d *Decider) CommitUserInputResponse(ctx context.Context, input UserInputResponseInput) (CommittedUserInputResponse, error) {
	target, err := d.userInput.ResolveTarget(ctx, userinput.ResolveInput{
		BotID:                  input.BotID,
		SessionID:              input.ThreadID,
		ExplicitID:             runreq.FirstNonEmpty(input.ExplicitID, input.UserInputID),
		ReplyExternalMessageID: input.ReplyExternalMessageID,
	})
	if err != nil {
		return CommittedUserInputResponse{}, err
	}

	isProcessLocalExternalAgent, err := d.isExternalAgentUserInputSession(ctx, runreq.FirstNonEmpty(target.SessionID, input.ThreadID))
	if err != nil {
		return CommittedUserInputResponse{}, err
	}
	if isProcessLocalExternalAgent {
		if err := d.authorizeExternalAgentUserInputResponse(ctx, target, input); err != nil {
			return CommittedUserInputResponse{}, err
		}
	}
	if !isProcessLocalExternalAgent {
		ctx = workspace.WithWorkspaceTarget(ctx, target.WorkspaceTargetID)
	}
	if isProcessLocalExternalAgent && !d.userInput.CanRespond(target) {
		if _, err := d.userInput.Cancel(ctx, userinput.CancelInput{
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
	var activePrompt *activeprompt.Subscription
	if isProcessLocalExternalAgent && !input.SuppressActivePromptAttach {
		activePrompt, _ = d.activePrompts.Subscribe(
			runreq.FirstNonEmpty(target.BotID, input.BotID),
			runreq.FirstNonEmpty(target.SessionID, input.ThreadID),
		)
	}

	var resolved userinput.Request
	if input.Canceled {
		resolved, err = d.userInput.Cancel(ctx, userinput.CancelInput{
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
					activePrompt.Release()
				}
				return CommittedUserInputResponse{}, err
			}
		}
		resolved, err = d.userInput.Submit(ctx, userinput.SubmitInput{
			RequestID:              target.ID,
			ActorChannelIdentityID: input.ActorChannelIdentityID,
			Answers:                answers,
		})
	}
	if err != nil {
		if activePrompt != nil {
			activePrompt.Release()
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

// isExternalAgentUserInputSession classifies a request by its session's runtime, the
// same way isExternalAgentToolApprovalSession does for approvals: a decision-waiter
// runtime consumes the answer inside the blocked turn, so it must not become
// a native model continuation.
func (d *Decider) isExternalAgentUserInputSession(ctx context.Context, sessionID string) (bool, error) {
	sess, err := d.sessions.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sessionpkg.UsesDecisionWaiter(sess), nil
}

func (d *Decider) authorizeExternalAgentUserInputResponse(ctx context.Context, target userinput.Request, input UserInputResponseInput) error {
	sessionID := runreq.FirstNonEmpty(target.SessionID, input.ThreadID)
	sess, err := d.sessions.Get(ctx, sessionID)
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
	if ok, err := d.botPermissions.HasBotPermission(ctx, botID, actorID, bots.PermissionWorkspaceExec); err != nil {
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

func WithLocalWebUserInputReplyTarget(req userinput.Request) userinput.Request {
	if strings.EqualFold(strings.TrimSpace(req.SourcePlatform), "web") && strings.TrimSpace(req.ReplyTarget) == "" {
		req.ReplyTarget = strings.TrimSpace(req.BotID)
	}
	return req
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
	activePrompt    *activeprompt.Subscription
	isExternalAgent bool
	ackOnly         bool
}

// Request is the user input request as it stands after the answer.
func (c CommittedUserInputResponse) Request() userinput.Request { return c.request }

// Input is the response the responder sent.
func (c CommittedUserInputResponse) Input() UserInputResponseInput { return c.input }

// RunID names the run the question parked, when the answer arrived as a
// command routed to its owner.
func (c CommittedUserInputResponse) RunID() string { return c.runID }

// RunHandle is the handle of the run the question parked.
func (c CommittedUserInputResponse) RunHandle() sessionruntime.RunHandle { return c.runHandle }

// IsExternalAgent says a waiter inside the running turn consumes the answer,
// so no continuation replays the call.
func (c CommittedUserInputResponse) IsExternalAgent() bool { return c.isExternalAgent }

// ActivePrompt is the live prompt the response stream reattached to, if any.
func (c CommittedUserInputResponse) ActivePrompt() *activeprompt.Subscription {
	return c.activePrompt
}

// AckOnly says the answer needs an acknowledgement and nothing else: its
// waiter is gone.
func (c CommittedUserInputResponse) AckOnly() bool { return c.ackOnly }

// ForRun names the run the question parked.
func (c CommittedUserInputResponse) ForRun(runID string, handle sessionruntime.RunHandle) CommittedUserInputResponse {
	c.runID, c.runHandle = runID, handle
	return c
}
