package decide

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/felinics/memoh/internal/agent/agentapi"
	"github.com/felinics/memoh/internal/agent/decision"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/internal/activeprompt"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/workspace"
)

// NewCommittedToolApproval is the committed response of an approval decided
// elsewhere: request as it stands after the decision, input as the decider
// sent it.
func NewCommittedToolApproval(request toolapproval.Request, input agentapi.ToolApprovalResponseInput, usesDecisionWaiter bool) CommittedToolApprovalResponse {
	return CommittedToolApprovalResponse{request: request, input: input, usesDecisionWaiter: usesDecisionWaiter}
}

func (d *Decider) CommitToolApprovalResponse(ctx context.Context, input agentapi.ToolApprovalResponseInput) (CommittedToolApprovalResponse, error) {
	target, err := d.toolApproval.ResolveTarget(ctx, toolapproval.ResolveInput{
		BotID:                  input.BotID,
		SessionID:              input.ThreadID,
		ExplicitID:             runreq.FirstNonEmpty(input.ExplicitID, input.ApprovalID),
		ReplyExternalMessageID: input.ReplyExternalMessageID,
	})
	if err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	isExternalAgent, err := d.isExternalAgentToolApprovalSession(ctx, target.SessionID)
	if err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	// Management tools retain their prepared operation inside the active call.
	// A response wakes that waiter; replaying the tool would prepare and execute
	// a second operation instead of the exact change the user approved.
	usesDecisionWaiter := isExternalAgent || isCapabilityManagementApproval(target)
	ctx = workspace.WithWorkspaceTarget(ctx, target.WorkspaceTargetID)
	if isExternalAgent {
		if err := d.authorizeExternalAgentToolApprovalResponse(ctx, target, input); err != nil {
			return CommittedToolApprovalResponse{}, err
		}
	} else if err := d.authorizeToolApprovalResponse(ctx, target, input); err != nil {
		return CommittedToolApprovalResponse{}, err
	}
	if usesDecisionWaiter && !d.toolApproval.CanRespond(target) {
		if _, err := d.toolApproval.Reject(ctx, target.ID, "", "tool approval expired: the requesting tool call is no longer waiting"); err != nil && !errors.Is(err, toolapproval.ErrAlreadyDecided) {
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
	var activePrompt *activeprompt.Subscription
	if isExternalAgent && !input.SuppressActivePromptAttach {
		activePrompt, _ = d.activePrompts.Subscribe(
			runreq.FirstNonEmpty(target.BotID, input.BotID),
			runreq.FirstNonEmpty(target.SessionID, input.ThreadID),
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
			target, err = d.toolApproval.ApproveOption(ctx, target.ID, input.ActorChannelIdentityID, input.Reason, optionID)
		default:
			if decision != "" && decision != "reject" && decision != "rejected" {
				err = fmt.Errorf("%w: decision %q does not match reject option %q", toolapproval.ErrOptionUnavailable, input.Decision, optionID)
				break
			}
			target, err = d.toolApproval.RejectOption(ctx, target.ID, input.ActorChannelIdentityID, input.Reason, optionID)
		}
	} else {
		switch decision {
		case "approve", "approved":
			target, err = d.toolApproval.Approve(ctx, target.ID, input.ActorChannelIdentityID, input.Reason)
		case "reject", "rejected":
			target, err = d.toolApproval.Reject(ctx, target.ID, input.ActorChannelIdentityID, input.Reason)
		default:
			err = fmt.Errorf("unknown tool approval decision %q", input.Decision)
		}
	}
	if err != nil {
		if activePrompt != nil {
			activePrompt.Release()
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

func (d *Decider) isExternalAgentToolApprovalSession(ctx context.Context, sessionID string) (bool, error) {
	sess, err := d.sessions.Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return sessionpkg.UsesDecisionWaiter(sess), nil
}

func (d *Decider) authorizeExternalAgentToolApprovalResponse(ctx context.Context, target toolapproval.Request, input agentapi.ToolApprovalResponseInput) error {
	sessionID := runreq.FirstNonEmpty(target.SessionID, input.ThreadID)
	sess, err := d.sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	if !sessionpkg.UsesDecisionWaiter(sess) {
		return d.authorizeToolApprovalResponse(ctx, target, input)
	}
	botID := runreq.FirstNonEmpty(target.BotID, input.BotID)
	if strings.TrimSpace(sess.BotID) != "" && strings.TrimSpace(botID) != "" && sess.BotID != botID {
		return toolapproval.ErrForbidden
	}
	if strings.TrimSpace(botID) == "" {
		botID = sess.BotID
	}
	target.BotID = botID
	actorID := runreq.FirstNonEmpty(input.ActorUserID, input.ActorChannelIdentityID)
	if actorID == "" {
		return toolapproval.ErrForbidden
	}
	runtimeOwnerID := runreq.MetadataString(runreq.RuntimeSessionMeta(sess), "runtime_owner_account_id")
	if runtimeOwnerID == "" {
		return toolapproval.ErrForbidden
	}
	// The runtime owner has no standing beyond their live grants: a revoked
	// or offboarded owner must lose approval authority at decision time, so
	// every actor — owner included — passes the permission check.
	return d.authorizeToolApprovalResponse(ctx, target, input)
}

func (d *Decider) authorizeToolApprovalResponse(ctx context.Context, target toolapproval.Request, input agentapi.ToolApprovalResponseInput) error {
	target.BotID = runreq.FirstNonEmpty(target.BotID, input.BotID)
	allowed, err := d.CanDecideToolApproval(ctx, target, agentapi.ToolApprovalPrincipal{
		UserID:            input.ActorUserID,
		ChannelIdentityID: input.ActorChannelIdentityID,
	})
	if err != nil {
		return err
	}
	if !allowed {
		return toolapproval.ErrForbidden
	}
	return nil
}

func WithLocalWebReplyTarget(req toolapproval.Request) toolapproval.Request {
	if strings.EqualFold(strings.TrimSpace(req.SourcePlatform), "web") && strings.TrimSpace(req.ReplyTarget) == "" {
		req.ReplyTarget = strings.TrimSpace(req.BotID)
	}
	return req
}

func RejectedToolResultText(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "tool execution rejected by user"
	}
	return "tool execution rejected by user: " + reason
}

type CommittedToolApprovalResponse struct {
	request            toolapproval.Request
	input              agentapi.ToolApprovalResponseInput
	runID              string
	runHandle          sessionruntime.RunHandle
	usesDecisionWaiter bool
	activePrompt       *activeprompt.Subscription
	ackOnly            bool
}

// Request is the approval request as it stands after the decision.
func (c CommittedToolApprovalResponse) Request() toolapproval.Request { return c.request }

// Input is the response the decider sent.
func (c CommittedToolApprovalResponse) Input() agentapi.ToolApprovalResponseInput { return c.input }

// RunID names the run the decision parked, when the decision arrived as a
// command routed to its owner.
func (c CommittedToolApprovalResponse) RunID() string { return c.runID }

// RunHandle is the handle of the run the decision parked.
func (c CommittedToolApprovalResponse) RunHandle() sessionruntime.RunHandle { return c.runHandle }

// UsesDecisionWaiter says a waiter inside the running call consumes the
// decision, so no continuation replays the call.
func (c CommittedToolApprovalResponse) UsesDecisionWaiter() bool { return c.usesDecisionWaiter }

// ActivePrompt is the live prompt the response stream reattached to, if any.
func (c CommittedToolApprovalResponse) ActivePrompt() *activeprompt.Subscription {
	return c.activePrompt
}

// AckOnly says the decision needs an acknowledgement and nothing else: its
// waiter is gone.
func (c CommittedToolApprovalResponse) AckOnly() bool { return c.ackOnly }

// ForRun names the run the decision parked.
func (c CommittedToolApprovalResponse) ForRun(runID string, handle sessionruntime.RunHandle) CommittedToolApprovalResponse {
	c.runID, c.runHandle = runID, handle
	return c
}
