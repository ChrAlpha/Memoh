package contextfrag

import sdk "github.com/memohai/twilight-ai/sdk"

type Kind string

const (
	KindSystemPrompt         Kind = "system_prompt"
	KindSystemPolicy         Kind = "system_policy"
	KindBotIdentity          Kind = "bot_identity"
	KindWorkspaceInstruction Kind = "workspace_instruction"
	KindPlatformIdentity     Kind = "platform_identity"
	KindToolUsage            Kind = "tool_usage"
	KindConversationEvent    Kind = "conversation_event"
	KindCurrentUserMessage   Kind = "current_user_message"
	KindAttachmentRef        Kind = "attachment_ref"
	KindNativeImage          Kind = "native_image"
	KindHookContext          Kind = "hook_context"
	KindBackgroundSummary    Kind = "background_summary"
	KindACPContext           Kind = "acp_context"
	KindMemoryRecall         Kind = "memory_recall"
	KindConversationSummary  Kind = "conversation_summary"
)

const CurrentSchemaVersion = 1

type Intent string

const (
	IntentRunConfigPreProvider Intent = "run_config_pre_provider"
	IntentCompactionCandidates Intent = "compaction_candidates"
	IntentDiscussReply         Intent = "discuss_reply"
	IntentACPRuntimePrompt     Intent = "acp_runtime_prompt"
)

type RenderTarget string

const (
	RenderSDKMessages      RenderTarget = "sdk_messages"
	RenderCompactionPrompt RenderTarget = "compaction_prompt"
	RenderACPFullContext   RenderTarget = "acp_full_context"
	RenderAuditManifest    RenderTarget = "audit_manifest"
)

const (
	SchemaContextManifest = "context_manifest"
	SchemaContextFrag     = "context_frag"
	SchemaContextRef      = "context_ref"
	SchemaContextEdit     = "context_edit"
	SchemaSummaryCoverage = "summary_coverage"
	SchemaRenderPolicy    = "render_policy"
)

const (
	HashAlgoSHA256             = "sha256"
	HashScopeCanonicalFragment = "canonical_fragment"
	HashScopeSourcePayload     = "source_payload"
)

type SchemaVersion struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type ContentRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type ContextRef struct {
	Namespace   string        `json:"namespace"`
	ID          string        `json:"id"`
	Version     int           `json:"version,omitempty"`
	Range       *ContentRange `json:"range,omitempty"`
	HashAlgo    string        `json:"hash_algo,omitempty"`
	ContentHash string        `json:"content_hash,omitempty"`
	HashScope   string        `json:"hash_scope,omitempty"`
	Schema      string        `json:"schema"`
	Durability  RefDurability `json:"durability,omitempty"`
}

type FragmentHash struct {
	Algo  string `json:"algo"`
	Scope string `json:"scope"`
	Value string `json:"value"`
}

type RefDurability string

const (
	RefDurable   RefDurability = "durable"
	RefSynthetic RefDurability = "synthetic"
	RefDebug     RefDurability = "debug"
)

type Slot string

const (
	SlotSystem                    Slot = "system"
	SlotBeforeHistory             Slot = "before_history"
	SlotHistory                   Slot = "history"
	SlotAfterHistoryBeforeCurrent Slot = "after_history_before_current"
	SlotCurrentUser               Slot = "current_user"
	SlotAfterCurrent              Slot = "after_current"
)

type CacheClass string

const (
	CacheStable  CacheClass = "stable"
	CacheDynamic CacheClass = "dynamic"
	CacheNever   CacheClass = "never"
)

type TrustLevel string

const (
	TrustSystem    TrustLevel = "system"
	TrustWorkspace TrustLevel = "workspace"
	TrustUser      TrustLevel = "user"
	TrustExternal  TrustLevel = "external"
)

type OverflowAction string

const (
	OverflowKeep      OverflowAction = "keep"
	OverflowTrim      OverflowAction = "trim"
	OverflowSummarize OverflowAction = "summarize"
	OverflowDrop      OverflowAction = "drop"
)

type BudgetPolicy struct {
	MaxTokens int            `json:"max_tokens,omitempty"`
	MaxChars  int            `json:"max_chars,omitempty"`
	Overflow  OverflowAction `json:"overflow,omitempty"`
}

type RenderFormat string

const (
	RenderPlainText  RenderFormat = "plain_text"
	RenderMarkdown   RenderFormat = "markdown"
	RenderSDKMessage RenderFormat = "sdk_message"
	RenderNativePart RenderFormat = "native_part"
)

type RenderPolicy struct {
	Format RenderFormat `json:"format,omitempty"`
	Anchor string       `json:"anchor,omitempty"`
}

type Provenance struct {
	Source    string `json:"source,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	Collector string `json:"collector,omitempty"`
	Index     int    `json:"index,omitempty"`
}

type AttentionReason string

const (
	AttentionDirect    AttentionReason = "direct"
	AttentionMention   AttentionReason = "mention"
	AttentionReply     AttentionReason = "reply"
	AttentionCommand   AttentionReason = "command"
	AttentionSchedule  AttentionReason = "schedule"
	AttentionHeartbeat AttentionReason = "heartbeat"
	AttentionPassive   AttentionReason = "passive"
)

type Scope struct {
	BotID                     string            `json:"bot_id,omitempty"`
	ChatID                    string            `json:"chat_id,omitempty"`
	SessionID                 string            `json:"session_id,omitempty"`
	TurnID                    string            `json:"turn_id,omitempty"`
	ViewHeadTurnID            string            `json:"view_head_turn_id,omitempty"`
	TurnMessageSeq            int64             `json:"turn_message_seq,omitempty"`
	SessionMode               string            `json:"session_mode,omitempty"`
	RuntimeType               string            `json:"runtime_type,omitempty"`
	ChannelIdentityID         string            `json:"channel_identity_id,omitempty"`
	DisplayName               string            `json:"display_name,omitempty"`
	Platform                  string            `json:"platform,omitempty"`
	ConversationType          string            `json:"conversation_type,omitempty"`
	ConversationName          string            `json:"conversation_name,omitempty"`
	ReplyTarget               string            `json:"reply_target,omitempty"`
	CurrentMessageID          string            `json:"current_message_id,omitempty"`
	EventID                   string            `json:"event_id,omitempty"`
	ReplyToMessageID          string            `json:"reply_to_message_id,omitempty"`
	ReplySender               string            `json:"reply_sender,omitempty"`
	MentionsBot               bool              `json:"mentions_bot,omitempty"`
	RepliesToBot              bool              `json:"replies_to_bot,omitempty"`
	ForwardMessageID          string            `json:"forward_message_id,omitempty"`
	ForwardFromUserID         string            `json:"forward_from_user_id,omitempty"`
	ForwardFromConversationID string            `json:"forward_from_conversation_id,omitempty"`
	Attention                 []AttentionReason `json:"attention,omitempty"`
	Metadata                  map[string]string `json:"metadata,omitempty"`
}

type PartType string

const (
	PartText       PartType = "text"
	PartSDKMessage PartType = "sdk_message"
	PartImage      PartType = "image"
)

type ImageRef struct {
	MediaType string `json:"media_type,omitempty"`
	Source    string `json:"source,omitempty"`
}

type Part struct {
	Type       PartType       `json:"type"`
	Text       string         `json:"text,omitempty"`
	Image      ImageRef       `json:"image,omitempty"`
	Message    *sdk.Message   `json:"message,omitempty"`
	ImagePart  *sdk.ImagePart `json:"image_part,omitempty"`
	SDKMessage *sdk.Message   `json:"-"`
	SDKImage   *sdk.ImagePart `json:"-"`
}

type ContextFrag struct {
	ID         string           `json:"id"`
	Ref        ContextRef       `json:"ref,omitempty"`
	Kind       Kind             `json:"kind"`
	Role       sdk.MessageRole  `json:"role,omitempty"`
	Slot       Slot             `json:"slot"`
	Priority   int              `json:"priority,omitempty"`
	CacheClass CacheClass       `json:"cache_class,omitempty"`
	Trust      TrustLevel       `json:"trust,omitempty"`
	Scope      Scope            `json:"scope,omitempty"`
	Budget     BudgetPolicy     `json:"budget,omitempty"`
	Render     RenderPolicy     `json:"render,omitempty"`
	Provenance Provenance       `json:"provenance,omitempty"`
	Coverage   *SummaryCoverage `json:"coverage,omitempty"`
	Parts      []Part           `json:"parts,omitempty"`
}

type AssembledContext struct {
	Frags        []ContextFrag   `json:"frags,omitempty"`
	System       string          `json:"system,omitempty"`
	Messages     []sdk.Message   `json:"-"`
	Query        string          `json:"query,omitempty"`
	InlineImages []sdk.ImagePart `json:"-"`
	Manifest     Manifest        `json:"manifest"`
}

type Manifest struct {
	SchemaVersions     []SchemaVersion     `json:"schema_versions,omitempty"`
	View               ManifestView        `json:"view,omitempty"`
	DynamicMutators    []DynamicMutator    `json:"dynamic_mutators,omitempty"`
	SlotPolicies       []SlotRenderPolicy  `json:"slot_policies,omitempty"`
	RenderedOutputs    []RenderedOutputRef `json:"rendered_outputs,omitempty"`
	EditTrace          []ContextEditTrace  `json:"edit_trace,omitempty"`
	CoverageTrace      []SummaryCoverage   `json:"coverage_trace,omitempty"`
	ContinuityGroups   []ContinuityGroup   `json:"continuity_groups,omitempty"`
	ValidationWarnings []ValidationWarning `json:"validation_warnings,omitempty"`
	Counts             ManifestCounts      `json:"counts"`
	Items              []ManifestItem      `json:"items,omitempty"`
}

type ManifestView string

const (
	ViewRunConfigPreProvider ManifestView = "run_config_pre_provider"
	ViewCompactionCandidates ManifestView = "compaction_candidates"
	ViewDiscussReply         ManifestView = "discuss_reply"
	ViewACPRuntimePrompt     ManifestView = "acp_runtime_prompt"
)

type DynamicMutator string

const (
	DynamicMutatorPromptCache         DynamicMutator = "prompt_cache"
	DynamicMutatorInjectCh            DynamicMutator = "inject_ch"
	DynamicMutatorReadMedia           DynamicMutator = "read_media"
	DynamicMutatorBeforeModelCallHook DynamicMutator = "before_model_call_hook"
	DynamicMutatorBackgroundSummary   DynamicMutator = "background_summary"
	DynamicMutatorMidTaskPrune        DynamicMutator = "mid_task_prune"
)

type ManifestCounts struct {
	Fragments int `json:"fragments"`
	Messages  int `json:"messages"`
	Images    int `json:"images"`
	TextBytes int `json:"text_bytes"`
}

type ManifestItem struct {
	ID         string          `json:"id"`
	Ref        ContextRef      `json:"ref,omitempty"`
	Kind       Kind            `json:"kind"`
	Slot       Slot            `json:"slot"`
	Role       sdk.MessageRole `json:"role,omitempty"`
	Priority   int             `json:"priority,omitempty"`
	CacheClass CacheClass      `json:"cache_class,omitempty"`
	Trust      TrustLevel      `json:"trust,omitempty"`
	Source     string          `json:"source,omitempty"`
	SourceID   string          `json:"source_id,omitempty"`
	Collector  string          `json:"collector,omitempty"`
	PartTypes  []PartType      `json:"part_types,omitempty"`
	TextBytes  int             `json:"text_bytes,omitempty"`
	ImageCount int             `json:"image_count,omitempty"`
	Scope      Scope           `json:"scope,omitempty"`
	Budget     BudgetPolicy    `json:"budget,omitempty"`
}

type SlotRenderPolicy struct {
	Slot          Slot   `json:"slot"`
	Order         string `json:"order,omitempty"`
	DedupeBy      string `json:"dedupe_by,omitempty"`
	CoverageAware bool   `json:"coverage_aware,omitempty"`
	Target        string `json:"target"`
}

type RenderedOutputRef struct {
	Target string       `json:"target"`
	Slot   Slot         `json:"slot,omitempty"`
	Refs   []ContextRef `json:"refs,omitempty"`
}

type ContextEditOp string

const (
	EditAppend   ContextEditOp = "append"
	EditReplace  ContextEditOp = "replace"
	EditRemove   ContextEditOp = "remove"
	EditCover    ContextEditOp = "cover"
	EditAnnotate ContextEditOp = "annotate"
)

type ContextEdit struct {
	EditID        string            `json:"edit_id"`
	Slot          Slot              `json:"slot"`
	Op            ContextEditOp     `json:"op"`
	Refs          []ContextRef      `json:"refs,omitempty"`
	Payload       []ContextFrag     `json:"payload,omitempty"`
	Preconditions EditPreconditions `json:"preconditions,omitempty"`
	Schema        SchemaVersion     `json:"schema"`
}

type EditPreconditions struct {
	ExpectedRevision string            `json:"expected_revision,omitempty"`
	MaxSequence      int64             `json:"max_sequence,omitempty"`
	ExpectedHashes   map[string]string `json:"expected_hashes,omitempty"`
}

type ContextEditTrace struct {
	EditID string        `json:"edit_id,omitempty"`
	Op     ContextEditOp `json:"op,omitempty"`
	Slot   Slot          `json:"slot,omitempty"`
	Refs   []ContextRef  `json:"refs,omitempty"`
}

type SummaryCoverage struct {
	CoverageID   string        `json:"coverage_id"`
	SummaryRef   ContextRef    `json:"summary_ref"`
	CoveredRefs  []ContextRef  `json:"covered_refs,omitempty"`
	TraceFragIDs []string      `json:"trace_frag_ids,omitempty"`
	Schema       SchemaVersion `json:"schema"`
}

type ContinuityGroup struct {
	ID               string       `json:"id"`
	Kind             string       `json:"kind"`
	Provider         string       `json:"provider,omitempty"`
	ModelFamily      string       `json:"model_family,omitempty"`
	Refs             []ContextRef `json:"refs,omitempty"`
	MustKeepTogether bool         `json:"must_keep_together,omitempty"`
	MustKeepRaw      bool         `json:"must_keep_raw,omitempty"`
	MustKeepOrder    bool         `json:"must_keep_order,omitempty"`
	MustBeComplete   bool         `json:"must_be_complete,omitempty"`
}

type ValidationWarning struct {
	Code    string     `json:"code"`
	Message string     `json:"message,omitempty"`
	Ref     ContextRef `json:"ref,omitempty"`
}

type ConflictKind string

const (
	ConflictMissingRef          ConflictKind = "missing_ref"
	ConflictContentHashMismatch ConflictKind = "content_hash_mismatch"
	ConflictInvalidSchema       ConflictKind = "invalid_schema"
)

type ContextConflict struct {
	Kind     ConflictKind `json:"kind"`
	Key      string       `json:"key,omitempty"`
	Expected string       `json:"expected,omitempty"`
	Actual   string       `json:"actual,omitempty"`
}
