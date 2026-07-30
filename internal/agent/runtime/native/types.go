package native

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/agent/background"
	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/event"
	tools "github.com/memohai/memoh/internal/agent/tool"
)

// SessionContext carries request-scoped identity and routing information.
type SessionContext struct {
	BotID               string
	ChatID              string
	SessionID           string
	UserID              string
	ChannelIdentityID   string
	CurrentPlatform     string
	ReplyTarget         string
	ConversationType    string
	Timezone            string
	TimezoneLocation    *time.Location
	SessionToken        string //nolint:gosec // carries session credential material at runtime
	WorkspaceTargetID   string
	WorkspaceTargetKind string
	WorkspaceTargetName string
	IsSubagent          bool
}

// BotInfo is service-owned bot metadata injected into the system prompt.
type BotInfo struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

// SkillEntry represents a skill loaded from the bot container.
type SkillEntry struct {
	Name        string
	Description string
	Content     string
	Path        string
	Metadata    map[string]any
}

// Schedule represents a scheduled task definition.
type Schedule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Pattern     string `json:"pattern"`
	MaxCalls    *int   `json:"maxCalls,omitempty"`
	Command     string `json:"command"`
}

// LoopDetectionConfig controls loop detection behavior.
type LoopDetectionConfig struct {
	Enabled bool
}

type ContextStepSelectionInput struct {
	Scope               contextfrag.Scope
	InitialMessageCount int
	Messages            []sdk.Message
	BudgetMaxTokens     int
	// RecentProtectTokens carries the run's recent-protection window override
	// so step reselection resolves the same window as the provider view. Nil
	// uses the view default; a pointer to zero disables the window.
	RecentProtectTokens *int
	// KeepRecentToolResults keeps the newest N complete tool cycles intact
	// and truncates older bulky tool results to a size summary; <= 0 disables
	// content truncation.
	KeepRecentToolResults int
	// MinMessages gates content truncation on total provider message count.
	MinMessages int
}

type ContextStepSelectionResult struct {
	Messages    []sdk.Message
	Dropped     int
	Truncated   int
	DropReasons map[string]int
	FatalError  error
}

type ContextStepReselector func(context.Context, ContextStepSelectionInput) ContextStepSelectionResult

// InjectMessage carries a user message to be injected into a running agent
// stream between tool rounds via the PrepareStep hook.
type InjectMessage struct {
	Text            string
	HeaderifiedText string
	// ImageParts carries inline images (data URL or public URL) to attach
	// alongside the injected text when the model supports vision input.
	ImageParts []sdk.ImagePart
}

// RunConfig holds everything needed for a single agent invocation.
type RunConfig struct {
	// RunID is the stable identity allocated by durable admission for this
	// invocation. Direct callers without admission receive one at the
	// application creation boundary before the native runtime starts.
	RunID                       string
	Model                       *sdk.Model
	CurrentModelUUID            string
	CurrentModelID              string
	CurrentModelProvider        string
	ForkContext                 *tools.MessageSnapshot
	ForkContextSourceMessageIDs []string
	ReasoningEffort             string
	ReasoningActive             bool
	ReasoningDisabled           bool
	ReasoningAdaptive           bool
	ReasoningOffEffort          string
	ChatCompletionsCompat       string
	Messages                    []sdk.Message
	Query                       string
	System                      string
	// ContextSourceFrags is the first-class context carrier: the collected
	// source fragments produced at resolve time. When present, the provider
	// view selects, places and renders from them and the legacy
	// System/Messages fields exist only as render outputs and fallback.
	ContextSourceFrags       []contextfrag.ContextFrag
	ContextSourceWarnings    []contextfrag.ValidationWarning
	ContextFrags             []contextfrag.ContextFrag
	ContextManifest          contextfrag.Manifest
	ContextToolDefs          []contextfrag.ToolDefAccounting
	ContextToolDefsResolved  bool
	ContextScope             contextfrag.Scope
	ContextQueryMaterialized bool
	// ContextCurrentUserMessageIndex identifies a current request already
	// materialized in Messages, as on the pipeline path.
	ContextCurrentUserMessageIndex *int
	ContextToolUsage               string
	ContextToolUsageFrags          []contextfrag.ContextFrag
	ContextDynamicMutators         []contextfrag.DynamicMutator
	ContextBudgetMaxTokens         int
	// ContextRecentProtectTokens overrides the recent-protection window the
	// provider view applies under budget pressure: the newest droppable
	// history within this many estimated tokens survives trimming. Nil uses
	// the view default; a pointer to zero disables the window.
	ContextRecentProtectTokens   *int
	ContextHistoryTokenEstimates []int
	ContextTrimmableMessages     int
	ContextCachePlan             contextfrag.CachePlan
	ContextToolExchangePolicy    *contextfrag.ToolExchangePolicy
	ContextMemoryText            string
	ContextMemoryHookText        string
	ContextHookText              string
	ContextMutations             *contextfrag.MutationLedger
	ContextLifecycle             *contextfrag.LifecycleHolder
	ContextStepReselector        ContextStepReselector
	contextStepFailure           func(error)
	SessionType                  string
	LiveToolStream               bool
	CanRequestUserInput          bool
	SupportsImageInput           bool
	SupportsToolCall             bool
	InlineImages                 []sdk.ImagePart
	Identity                     SessionContext
	Bot                          BotInfo
	Skills                       []SkillEntry
	LoopDetection                LoopDetectionConfig
	Retry                        RetryConfig

	// PromptCacheTTL controls prompt caching for this run. Empty or
	// unrecognized values default to 5m. Use "1h" for the long-cache tier
	// or "off" to disable caching entirely. The TTL is honored only when
	// the resolved model's vendor implements prompt caching (currently
	// Anthropic Messages); for other vendors the value is ignored.
	PromptCacheTTL string

	// InjectCh receives user messages to inject between tool rounds.
	// When non-nil, a PrepareStep hook drains this channel and appends
	// user messages to the conversation before the next LLM call.
	InjectCh <-chan InjectMessage

	// InjectedRecorder is called each time a message is injected via
	// PrepareStep, recording the headerified text and the number of SDK
	// output messages that preceded the injection. Used by the resolver
	// to interleave injected messages at the correct position in storeRound.
	InjectedRecorder func(headerifiedText string, insertAfter int)

	// BackgroundManager provides access to the background task system.
	// When non-nil, the agent loop refreshes running task summaries at step
	// boundaries while tools handle waiting and result inspection.
	BackgroundManager *background.Manager

	ToolApprovalHandler func(ctx context.Context, call sdk.ToolCall) (sdk.ToolApprovalResult, error)
}

// GenerateResult holds the result of a non-streaming agent invocation.
type GenerateResult struct {
	Messages    []sdk.Message
	Text        string
	Attachments []FileAttachment
	Reactions   []ReactionItem
	Speeches    []SpeechItem
	Usage       *sdk.Usage
}

// FileAttachment, ReactionItem and SpeechItem live in the event leaf package
// (they ride on StreamEvent); aliased here for source compatibility.
type (
	FileAttachment = event.FileAttachment
	ReactionItem   = event.ReactionItem
	SpeechItem     = event.SpeechItem
)

// SystemFile is a file loaded from the bot container for prompt generation.
type SystemFile struct {
	Filename string
	Content  string
}

// ModelConfig holds provider and model information resolved from DB.
type ModelConfig struct {
	ModelID         string
	ClientType      string
	APIKey          string //nolint:gosec // carries provider credential material at runtime
	CodexAccountID  string
	BaseURL         string
	HTTPClient      *http.Client
	ReasoningConfig *ReasoningConfig
}

// ReasoningConfig controls extended thinking/reasoning behavior.
type ReasoningConfig struct {
	Enabled bool
	Effort  string
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}
