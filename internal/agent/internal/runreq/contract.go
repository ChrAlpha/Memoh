package runreq

import (
	"encoding/json"

	"github.com/felinics/memoh/internal/agent/agentapi"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// ChatRequest is the application-layer input used while orchestrating a chat
// turn. Transport callers should prefer turn.StartTurnCommand; the additional
// channel and function fields below are strictly in-process runtime state.
type ChatRequest struct {
	// ShutdownResume rebuilds saved history without the transient channel pipeline.
	ShutdownResume bool `json:"-"`
	// OnModelPreferenceSettled releases subsequent picker writes once this
	// turn can no longer overwrite them. It does not acknowledge generation.
	OnModelPreferenceSettled func() `json:"-"`

	BotID    string `json:"-"`
	ChatID   string `json:"-"`
	ThreadID string `json:"-"`
	// RunID is the server-minted identity of this turn's run. Downstream state
	// that must agree on "which turn is this" — the compaction barrier, the ACP
	// session, interactive tool headers — keys on it.
	RunID string `json:"-"`
	// RunHandle is the server-owned execution capability for durable step and
	// queue commits. Transport callers cannot supply it.
	RunHandle sessionruntime.RunHandle `json:"-"`
	// TurnID and TurnPosition are the turn admission already allocated for this
	// run. They travel with the request so the persisted user turn lands under
	// the id the client was handed at run_accepted, instead of the history layer
	// minting a second one (SR-TURN-001). Empty means no admission decided the
	// turn — channel inbound and schedules — and history allocates it.
	TurnID                    string            `json:"-"`
	TurnPosition              *int64            `json:"-"`
	Token                     string            `json:"-"`
	UserID                    string            `json:"-"`
	SourceChannelIdentityID   string            `json:"-"`
	DisplayName               string            `json:"-"`
	RouteID                   string            `json:"-"`
	ChatToken                 string            `json:"-"`
	ExternalMessageID         string            `json:"-"`
	ReplyTarget               string            `json:"-"`
	ConversationType          string            `json:"-"`
	ConversationName          string            `json:"-"`
	SourceReplyToMessageID    string            `json:"-"`
	ReplySender               string            `json:"-"`
	ReplyPreview              string            `json:"-"`
	ReplyAttachments          []turn.Attachment `json:"-"`
	MentionsBot               bool              `json:"-"`
	RepliesToBot              bool              `json:"-"`
	ForwardMessageID          string            `json:"-"`
	ForwardFromUserID         string            `json:"-"`
	ForwardFromConversationID string            `json:"-"`
	ForwardSender             string            `json:"-"`
	ForwardDate               int64             `json:"-"`
	UserMessagePersisted      bool              `json:"-"`
	PersistedUserMessageID    string            `json:"-"`
	// RequestStaysInContext keeps the persisted request row in the model's
	// history. A continuation whose instruction restates the task drops it so
	// the task appears once; one whose task lives only in that row keeps it.
	RequestStaysInContext     bool                  `json:"-"`
	ReusePersistedUserMessage bool                  `json:"-"`
	EventID                   string                `json:"-"`
	RawQuery                  string                `json:"-"`
	ModelQuery                string                `json:"-"`
	UserMessageKind           string                `json:"-"`
	UserVisibleText           string                `json:"-"`
	SkillActivation           *turn.SkillActivation `json:"-"`
	ToolHTTPURL               string                `json:"-"`
	SessionType               string                `json:"-"`
	RuntimeType               string                `json:"-"`
	SkipMemoryExtraction      bool                  `json:"-"`
	// TurnReplacement starts an admitted retry/edit attempt in the first
	// persistence transaction. All messages bind to their turn immediately;
	// later steps append normally, including across decisions and failures.
	TurnReplacement     *messagepkg.TurnReplacement `json:"-"`
	SkipTitleGeneration bool                        `json:"-"`
	ForceFreshRuntime   bool                        `json:"-"`
	// AgentCommand is the exact agent-command selector the Web admission layer
	// matched against a live ACP runtime. The session pool re-validates it
	// against the final session at prompt time; it never crosses the turn
	// transport (Web admission runs in-process).
	AgentCommand                 string `json:"-"`
	HistoryCutoffBeforeMessageID string `json:"-"`
	RequiredHistoryMessageID     string `json:"-"`
	// DiscussBatchMessageIDs are the history rows a discuss run answers: the
	// external input past the session's cover position. Only the admission
	// knows where that position was when the run took the session, and the
	// images those rows carry go to the model with them.
	DiscussBatchMessageIDs []string                  `json:"-"`
	WorkspaceTarget        *agentapi.WorkspaceTarget `json:"-"`

	// OutboundAssetCollector returns asset refs accumulated during outbound
	// streaming. It is never serialized across the turn transport.
	OutboundAssetCollector func() []turn.OutboundAssetRef `json:"-"`

	// InjectCh receives user messages between tool rounds. Remote transports
	// use turn.RunHandle.Inject instead.
	InjectCh <-chan turn.InjectMessage `json:"-"`
	// QueueSteerEnabled enables the fenced native queue consumer.
	QueueSteerEnabled bool `json:"-"`
	StepIndexOffset   int  `json:"-"`

	Query             string                       `json:"query"`
	Model             string                       `json:"model,omitempty"`
	RuntimeModelID    string                       `json:"runtime_model_id,omitempty"`
	Provider          string                       `json:"provider,omitempty"`
	ReasoningEffort   string                       `json:"reasoning_effort,omitempty"`
	WorkspaceTargetID string                       `json:"workspace_target_id,omitempty"`
	Channels          []string                     `json:"channels,omitempty"`
	CurrentChannel    string                       `json:"current_channel,omitempty"`
	Messages          []turn.ModelMessage          `json:"messages,omitempty"`
	Attachments       []turn.Attachment            `json:"attachments,omitempty"`
	RequestedSkills   []turn.RequestedSkillContext `json:"-"`
}

// The canonical turn value objects are re-exported inside application so the
// orchestration code can use them without maintaining a second DTO family.
type (
	ChatAttachment        = turn.Attachment
	ModelMessage          = turn.ModelMessage
	ContentPart           = turn.ContentPart
	ToolCall              = turn.ToolCall
	ToolCallFunction      = turn.ToolCallFunction
	RequestedSkillContext = turn.RequestedSkillContext
	SkillActivation       = turn.SkillActivation
	SkillActivationSkill  = turn.SkillActivationSkill
	OutboundAssetRef      = turn.OutboundAssetRef
	InjectMessage         = turn.InjectMessage
	AssistantOutput       = turn.AssistantOutput
)

// UserMessageKindSkillActivation marks a user message that activates a skill.
const UserMessageKindSkillActivation = turn.UserMessageKindSkillActivation

// NewTextContent encodes text as a model-message content value.
func NewTextContent(text string) json.RawMessage {
	return turn.NewTextContent(text)
}
