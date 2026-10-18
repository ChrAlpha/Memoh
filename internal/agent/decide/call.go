package decide

import (
	"errors"
	"fmt"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/toolexec"
)

// CallKind names the decision an answered call came from. It only chooses the
// words of the error a malformed call reports.
type CallKind int

const (
	CallToolApproval CallKind = iota + 1
	CallUserInput
)

// Verdict is what the decision settled about the parked call.
type Verdict int

const (
	// VerdictUnknown is a decision whose status settles nothing the run can act
	// on.
	VerdictUnknown Verdict = iota
	// VerdictApproved says the call runs now; the decision holds no result yet.
	VerdictApproved
	// VerdictRejected says the call was refused; Result carries the refusal.
	VerdictRejected
	// VerdictAnswered says the user answered the question; Result carries the
	// answer.
	VerdictAnswered
)

// DecidedCall is a decision whose answer is durable, as whoever resumes the
// run needs it: where the run is, what the parked call was, and either the
// call to run or the result it already has. It is the one value an approval and
// an ask_user answer both hand over.
type DecidedCall struct {
	Kind    CallKind
	Verdict Verdict
	// Status is the decision's status, kept for the error a verdict that is
	// unknown reports.
	Status string
	// DecisionID names the stored decision row.
	DecisionID string

	ToolCallID string
	ToolName   string
	// ToolInput is the call's arguments, which an approved call runs with.
	ToolInput any
	// Result is the parked call's result when the decision produced it: the
	// refusal of a rejected approval, the answer to a question. An approved
	// call has none until it has run.
	Result sdk.ToolResultPart
	// SkipMemoryExtraction says the result is stored as a tool result and not
	// as a new user message, so no write may schedule a user-message memory
	// extraction.
	SkipMemoryExtraction bool

	// RunID and RunHandle name the run the decision parked, when the answer
	// arrived as a command routed to its owner.
	RunID     string
	RunHandle sessionruntime.RunHandle

	// BotID is the bot the response was sent to. ScopeBotID is the bot the
	// decision row names, falling back to BotID.
	BotID      string
	ScopeBotID string
	// ChatToken is the credential the response carried.
	ChatToken string
	// ChannelIdentityID is who the run resumes as: the identity the decision
	// row names, or the one that answered.
	ChannelIdentityID string
	SessionID         string
	SourcePlatform    string
	ReplyTarget       string
	ConversationType  string
	// WorkspaceTargetID is the Computer the parked call ran on.
	WorkspaceTargetID string
}

// Validate reports why the call cannot be resumed: its decision row is missing,
// or its status settles nothing.
func (c DecidedCall) Validate() error {
	if strings.TrimSpace(c.DecisionID) == "" {
		if c.Kind == CallUserInput {
			return errors.New("committed user input response is missing its request")
		}
		return errors.New("committed tool approval response is missing its request")
	}
	if c.Verdict == VerdictUnknown {
		return fmt.Errorf("committed tool approval has unexpected status %q", c.Status)
	}
	return nil
}

// ToolApprovalCall is the decided call of a committed approval.
func (d *Decider) ToolApprovalCall(committed CommittedToolApprovalResponse) DecidedCall {
	approval := WithLocalWebReplyTarget(committed.Request())
	input := committed.Input()
	call := DecidedCall{
		Kind:              CallToolApproval,
		Status:            approval.Status,
		DecisionID:        approval.ID,
		ToolCallID:        approval.ToolCallID,
		ToolName:          approval.ToolName,
		ToolInput:         approval.ToolInput,
		RunID:             committed.RunID(),
		RunHandle:         committed.RunHandle(),
		BotID:             input.BotID,
		ScopeBotID:        runreq.FirstNonEmpty(approval.BotID, input.BotID),
		ChatToken:         input.ChatToken,
		ChannelIdentityID: runreq.FirstNonEmpty(approval.ChannelIdentityID, input.ActorChannelIdentityID),
		SessionID:         approval.SessionID,
		SourcePlatform:    approval.SourcePlatform,
		ReplyTarget:       approval.ReplyTarget,
		ConversationType:  approval.ConversationType,
		WorkspaceTargetID: approval.WorkspaceTargetID,
	}
	switch approval.Status {
	case toolapproval.StatusApproved:
		call.Verdict = VerdictApproved
	case toolapproval.StatusRejected:
		call.Verdict = VerdictRejected
		call.Result = sdk.ToolResultPart{
			ToolCallID: approval.ToolCallID,
			ToolName:   approval.ToolName,
			Result:     toolexec.OutputFromValue(d.LimitToolResultText(RejectedToolResultText(input.Reason), approval.ToolName)),
			IsError:    true,
		}
	}
	return call
}

// UserInputCall is the decided call of a committed answer.
func (d *Decider) UserInputCall(committed CommittedUserInputResponse) DecidedCall {
	resolved := WithLocalWebUserInputReplyTarget(committed.Request())
	input := committed.Input()
	return DecidedCall{
		Kind:       CallUserInput,
		Verdict:    VerdictAnswered,
		Status:     resolved.Status,
		DecisionID: resolved.ID,
		ToolCallID: resolved.ToolCallID,
		ToolName:   resolved.ToolName,
		Result: sdk.ToolResultPart{
			ToolCallID: resolved.ToolCallID,
			ToolName:   resolved.ToolName,
			Result:     toolexec.OutputFromValue(d.LimitToolResultValue(resolved.Result, resolved.ToolName)),
			IsError:    false,
		},
		// The answer is stored as the ask_user call's result, not as a new user
		// message.
		SkipMemoryExtraction: true,
		RunID:                committed.RunID(),
		RunHandle:            committed.RunHandle(),
		BotID:                input.BotID,
		ScopeBotID:           runreq.FirstNonEmpty(resolved.BotID, input.BotID),
		ChatToken:            input.ChatToken,
		ChannelIdentityID:    runreq.FirstNonEmpty(resolved.ChannelIdentityID, input.ActorChannelIdentityID),
		SessionID:            resolved.SessionID,
		SourcePlatform:       resolved.SourcePlatform,
		ReplyTarget:          resolved.ReplyTarget,
		ConversationType:     resolved.ConversationType,
		WorkspaceTargetID:    resolved.WorkspaceTargetID,
	}
}
