// Package turn defines the application-level contract for starting and
// observing agent turns. It is the only agent surface Channel may depend
// on; it must not import Echo, fx, sqlc, conversation, or any channel package.
package turn

import (
	"context"
	"encoding/json"
	"errors"

	userinput "github.com/felinics/memoh/internal/agent/decision/input"
)

// ErrDuplicateTurn reports that a StartTurnCommand's (TeamID,
// IdempotencyKey) pair was already claimed by an earlier run. Callers
// handling platform webhook retries should treat it as successful
// delivery and drop the duplicate silently.
var ErrDuplicateTurn = errors.New("turn: duplicate idempotency key")

// ErrSessionBusy reports that the thread cannot take a new run now: it has a
// run in flight, or input queued ahead of the turn. An ingress parks the
// complete turn in the session's input queue and answers the sender that it is
// waiting; other callers retry the unchanged command.
//
// It is declared here rather than reused from the runtime because this package
// is the only agent surface Channel may import, and it must not depend on the
// runtime that produces the condition.
var ErrSessionBusy = errors.New("turn: thread already has a run in flight")

// ErrTeamNotServed reports that the service instance does not serve the
// command's team. The in-process runtime binds its database pool to the
// single self-hosted team, so commands for any other team must fail
// closed instead of silently reading and writing the default team's data.
var ErrTeamNotServed = errors.New("turn: team not served by this instance")

// Mode selects the turn orchestration path.
type Mode string

const ModeChat Mode = "chat"

// StartOrigin says where a start comes from, which decides what admission asks
// of it before the run exists.
type StartOrigin string

const (
	// OriginDirect is a turn someone started by acting: a message, a retry, an
	// edit. It is admitted at once, behind input already queued for the
	// session, and it takes the session over from an interrupted run.
	OriginDirect StartOrigin = ""
	// OriginPending is a batch pending itself starts from the queue it holds,
	// once it has put what the session is owed in order. It neither waits
	// behind the queued input it is starting nor is counted as new input, and
	// when it races another admission it surfaces ErrSessionBusy instead of
	// queueing again.
	OriginPending StartOrigin = "pending"
	// OriginPlatformQueue is a platform message the channel process takes out
	// of the queue, because it delivers the reply. It is owed to the session
	// as work pending holds, so admission puts it in the same order before it
	// starts: behind the continuation of an interrupted run, and not before the
	// workspace is ready. A session that cannot take it yet answers
	// ErrSessionBusy, and the message waits in the queue.
	OriginPlatformQueue StartOrigin = "platform_queue"
)

// StartTurnCommand is a pure-data command. Field set mirrors exactly what
// the channel inbound processor supplies today; function- and channel-typed fields are
// intentionally excluded — injection goes through RunHandle.Inject and
// outbound assets through RunHandle.AddOutboundAssets. A command that crosses
// a process or is stored travels as turnwire.StartTurn, not as this type's
// JSON.
type StartTurnCommand struct {
	// SchemaVersion is the command shape its producer built. A producer whose
	// command is sent or stored declares it; the reader refuses a version it
	// does not know (turnwire.StartTurnVersion).
	SchemaVersion int
	// Origin says where the start comes from; the zero value is a start
	// someone made by acting.
	Origin StartOrigin
	TeamID string // required; the service fails closed when empty
	Mode   Mode

	BotID                   string
	ChatID                  string
	ThreadID                string
	RouteID                 string
	Token                   string
	ChatToken               string
	UserID                  string
	SourceChannelIdentityID string
	DisplayName             string

	IdempotencyKey    string // derived from platform external message id
	ExternalMessageID string
	EventID           string

	AgentCommand    string
	Query           string
	ModelQuery      string
	UserMessageKind string
	UserVisibleText string
	Attachments     []Attachment

	ReplyTarget            string
	ConversationType       string
	ConversationName       string
	SourceReplyToMessageID string
	ReplySender            string
	ReplyPreview           string
	ReplyAttachments       []Attachment
	MentionsBot            bool
	RepliesToBot           bool

	ForwardMessageID          string
	ForwardFromUserID         string
	ForwardFromConversationID string
	ForwardSender             string
	ForwardDate               int64

	CurrentChannel string
	Channels       []string

	Model             string
	ReasoningEffort   string
	WorkspaceTargetID string

	SkillActivation      *SkillActivation
	RequestedSkills      []RequestedSkillContext
	SkipMemoryExtraction bool
	SkipTitleGeneration  bool
	UserMessagePersisted bool

	// Preceding are the messages this command's run answers together with its
	// own, which arrived before it: the earlier inputs of a queued batch. Each
	// is written as a user row of its own ahead of the run's request, in this
	// order, so the model reads them as it would had they been recorded as they
	// came, and the run's request is the last of them.
	Preceding []PrecedingMessage
}

// PrecedingMessage is one earlier message of a batch a run answers. It carries
// what a user row and its header are made of; where the message was said
// (channel, conversation, reply target) is the command's own.
type PrecedingMessage struct {
	// ID names the queue item the message waited as. Its row is written under
	// that id, once: a batch that starts again after a crash, or is released and
	// formed anew, finds the row it wrote.
	ID                      string
	ExternalMessageID       string
	UserID                  string
	SourceChannelIdentityID string
	DisplayName             string
	Query                   string
	UserVisibleText         string
	Attachments             []Attachment

	SourceReplyToMessageID string
	ReplySender            string
	ReplyPreview           string
	ReplyAttachments       []Attachment
	MentionsBot            bool
	RepliesToBot           bool

	ForwardMessageID          string
	ForwardFromUserID         string
	ForwardFromConversationID string
	ForwardSender             string
	ForwardDate               int64

	// ReceivedAtMs is when the platform delivered the message, in Unix
	// milliseconds; zero when unknown. The row's header carries this time, the
	// way a passive row's does.
	ReceivedAtMs int64
}

// ToolApprovalResponse resumes a thread's turn deferred on tool approval
// (RFC ResumeApprovalCommand).
type ToolApprovalResponse struct {
	ControlID              string
	BotID                  string
	ThreadID               string
	ActorChannelIdentityID string
	ActorUserID            string
	ApprovalID             string
	ExplicitID             string
	ReplyExternalMessageID string
	Decision               string
	// OptionID is the opaque agent-provided permission option selected by the
	// Web client. Empty retains the legacy binary approve/reject behavior.
	OptionID                   string
	Reason                     string
	ChatToken                  string
	SuppressActivePromptAttach bool
}

// UserInputResponse resumes a thread's turn deferred on ask_user
// (RFC ResumeUserInputCommand).
type UserInputResponse struct {
	ControlID                  string
	BotID                      string
	ThreadID                   string
	ActorChannelIdentityID     string
	ActorUserID                string
	UserInputID                string
	ExplicitID                 string
	ReplyExternalMessageID     string
	Answers                    []QuestionAnswer
	UILanguage                 string
	TextAnswer                 string
	Canceled                   bool
	Reason                     string
	ChatToken                  string
	SuppressActivePromptAttach bool
}

// Event is one element of a thread turn's event stream. Payload is the raw JSON
// chunk exactly as produced by the runtime; Kind is the parsed "type"
// field (best effort, empty when unparsable). Seq is monotonically
// increasing per run.
type Event struct {
	RunID    string
	TeamID   string
	ThreadID string
	Seq      int64
	Kind     string
	Payload  json.RawMessage
}

// RunHandle observes and steers one running turn. Events and Errs mirror
// the runtime's chunk/error channel pair; both close when the run ends.
type RunHandle interface {
	RunID() string
	Events() <-chan Event
	Errs() <-chan error
	Inject(ctx context.Context, msg InjectMessage) error
	AddOutboundAssets(refs []OutboundAssetRef)
	Cancel()
}

// Service starts and resumes turns. The application service implements this
// contract directly; cross-process transports expose the same contract. The
// eventCh parameters mirror the application's raw JSON stream for resumed
// turns and are not part of the serialized command shape.
type Service interface {
	StartTurn(ctx context.Context, cmd StartTurnCommand) (RunHandle, error)
	RespondToolApproval(ctx context.Context, input ToolApprovalResponse, eventCh chan<- json.RawMessage) error
	RespondUserInput(ctx context.Context, input UserInputResponse, eventCh chan<- json.RawMessage) error
	AdvancePlainTextUserInput(ctx context.Context, input userinput.AdvanceTextInput) (userinput.AdvanceTextResult, error)
}

// StopCommand targets the current durable run, including a parked decision.
// TeamID must match the runtime instance; channel ingress authorizes the actor.
type StopCommand struct{ TeamID, BotID, ThreadID string }

// Stopper supplements stream cancellation for runs whose output stream ended
// while waiting for a decision. Kept separate for alternate turn providers.
type Stopper interface {
	StopTurn(context.Context, StopCommand) (bool, error)
}
