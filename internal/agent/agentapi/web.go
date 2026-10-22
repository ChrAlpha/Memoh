package agentapi

import (
	"context"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/auth"
)

// WebAdmitInput is one Web submission as the WebSocket names it.
type WebAdmitInput struct {
	BotID        string
	SessionID    string
	InvocationID string
	// InitiatorID is the account the submission was made under.
	InitiatorID string
	// Payload is the canonical form of what the client sent. Its bytes decide
	// whether a repeated invocation is the same submit or a conflicting one.
	Payload []byte
	// Admission builds what subscribers see once the run leaves admitting: a
	// send's request user turn, or the boundary a retry or edit replaces. It
	// runs after ownership is established, so its writes are fenced.
	Admission func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error)
	// QueueTurn builds the complete turn of a send that may wait, and the
	// identities its credentials are issued for when it starts. It is called only
	// when the send does wait: the session has a run in flight, or input queued
	// ahead of it. A submission that cannot wait as plain input, such as a retry,
	// an edit or a skill activation, leaves it nil and meets ErrSessionBusy.
	QueueTurn func(context.Context) (turn.StartTurnCommand, *auth.RouteCredentials, error)
}

// WebAdmission is the run a Web submission was admitted as. The run id, its
// turn and the cursor it became observable at come from the session runtime,
// which is what makes them the same for every subscriber of the session.
//
// Only the value AdmitWebRun returned is valid for RunWebSend, RunWebRetry and
// RunWebEdit.
type WebAdmission interface {
	RunID() string
	TurnID() string
	TurnPosition() int64
	// Cursor is where the run became observable. A duplicate reserved nothing
	// and has none: where the session stands is what a subscription answers.
	Cursor() sessionruntime.Cursor
	// Duplicate means the invocation already had a run, owned here or
	// elsewhere, or already finished. Executing it again is the duplication
	// admission exists to prevent, so there is nothing to run.
	Duplicate() bool
	// Queued means the submission was not run: it waits in the session's queue
	// behind the run in flight and the input queued before it, and starts a run of
	// its own in its turn. QueuedItemID names it in the queue. A Duplicate queued
	// admission is a submission the queue already held or had taken.
	Queued() bool
	QueuedItemID() string
}

// WebRetryInput names the turn a Web retry replaces and who asks. The run that
// replaces it is the admission's, so the input carries none of the run's
// identity.
type WebRetryInput struct {
	BotID     string
	SessionID string
	// TargetTurnID names the round being replaced. It is distinct from the
	// admission's turn, which names the new turn this operation runs.
	TargetTurnID           string
	ActorChannelIdentityID string
	ActorUserID            string
	ChatToken              string
	Model                  string
	ReasoningEffort        string
	WorkspaceTargetID      string
	ToolHTTPURL            string
	// OnModelPreferenceSettled releases subsequent picker writes once this
	// turn's preference write-back has finished (issue #879). Same contract as
	// WebSendInput.OnModelPreferenceSettled.
	OnModelPreferenceSettled func()
}

// WebEditInput names the turn a Web edit replaces and its new text.
type WebEditInput struct {
	BotID     string
	SessionID string
	// TargetTurnID names the round being replaced. See WebRetryInput.
	TargetTurnID           string
	Text                   string
	Attachments            []turn.Attachment
	ActorChannelIdentityID string
	ActorUserID            string
	ChatToken              string
	Model                  string
	ReasoningEffort        string
	WorkspaceTargetID      string
	ToolHTTPURL            string
	// OnModelPreferenceSettled: see WebRetryInput.
	OnModelPreferenceSettled func()
}

// WorkspaceTarget is the immutable execution-location snapshot resolved for
// one request.
type WorkspaceTarget struct {
	TargetID string `json:"target_id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

// WebSendInput is a Web text send as the socket fills it in. The first call
// (persisting a skill activation's user turn ahead of the run) returns it with
// the user-message hook's rewrites applied; the second hands it to the run.
type WebSendInput struct {
	BotID                   string
	ChatID                  string
	ThreadID                string
	UserID                  string
	SourceChannelIdentityID string
	ExternalMessageID       string
	ConversationType        string
	CurrentChannel          string
	ReplyTarget             string
	Channels                []string

	Query           string
	ModelQuery      string
	RawQuery        string
	UserVisibleText string
	UserMessageKind string
	// Messages is the history the user-message hook rewrote. The socket never
	// sets it; it reads it back from the persist call.
	Messages          []turn.ModelMessage
	SkillActivation   *turn.SkillActivation
	RequestedSkills   []turn.RequestedSkillContext
	Attachments       []turn.Attachment
	WorkspaceTargetID string
	// WorkspaceTarget is read back like Messages.
	WorkspaceTarget *WorkspaceTarget

	UserMessagePersisted   bool
	PersistedUserMessageID string

	Token           string
	ChatToken       string
	Model           string
	ReasoningEffort string
	ToolHTTPURL     string
	AgentCommand    string

	SkipMemoryExtraction bool
	SkipTitleGeneration  bool
	// OnModelPreferenceSettled releases subsequent picker writes once this
	// turn can no longer overwrite them. It does not acknowledge generation.
	OnModelPreferenceSettled func()
}
