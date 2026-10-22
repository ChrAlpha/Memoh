package agentapi

import userinput "github.com/felinics/memoh/internal/agent/decision/input"

// ToolApprovalPrincipal is who decides an approval, or for whom an approval is
// rendered. Channel deciders without a bound account carry only their channel
// identity; grants are keyed on that identity, so it stays a valid subject.
type ToolApprovalPrincipal struct {
	UserID            string
	ChannelIdentityID string
}

// ToolApprovalResponseInput is one decider's answer to a tool approval, as the
// ingress received it.
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

// UserInputResponseInput is one responder's answer to a question the agent
// asked, as the ingress received it.
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
