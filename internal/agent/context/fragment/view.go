package contextfrag

// Intent names why a context is being built; RenderTarget names what it is
// rendered into. One intent may fan out to several render targets.
type Intent string

const (
	IntentRunConfigPreProvider Intent = "run_config_pre_provider"
	IntentCompactionCandidates Intent = "compaction_candidates"
	IntentDiscussReply         Intent = "discuss_reply"
	IntentACPRuntimePrompt     Intent = "acp_runtime_prompt"
)

func (i Intent) ManifestView() ManifestView {
	return ManifestView(i)
}

type RenderTarget string

const (
	RenderSDKMessages      RenderTarget = "sdk_messages"
	RenderCompactionPrompt RenderTarget = "compaction_prompt"
	RenderACPFullContext   RenderTarget = "acp_full_context"
	RenderAuditManifest    RenderTarget = "audit_manifest"
)

const (
	ViewCompactionCandidates ManifestView = "compaction_candidates"
	ViewDiscussReply         ManifestView = "discuss_reply"
	ViewACPRuntimePrompt     ManifestView = "acp_runtime_prompt"
)

// NormalizeContextRefs fills durable refs and canonical hashes for fragments
// coming from collectors, mirroring what Compile does for legacy inputs.
func NormalizeContextRefs(frags []ContextFrag) []ContextFrag {
	return normalizeContextRefs(frags)
}

// CachePlan is the placement-derived prompt cache shape handed to the
// provider call: the stable prefix identity plus how many leading rendered
// messages are cache-stable and may carry a cache breakpoint.
type CachePlan struct {
	StablePrefixHash   string `json:"stable_prefix_hash,omitempty"`
	StableMessageCount int    `json:"stable_message_count,omitempty"`
	// StablePrefixTokenEstimate is the plan-time estimate of everything the
	// message-level breakpoint covers (tool definitions, stable system
	// fragments, stable leading messages) — the denominator for judging how
	// much of the cacheable prefix provider-reported cache reads recovered.
	StablePrefixTokenEstimate int `json:"stable_prefix_token_estimate,omitempty"`
	// MidStableMessageCount asks for an additional breakpoint after this many
	// leading messages, so a prune or compaction that busts the tail of a
	// large stable span still hits the cached mid-span prefix. Zero means the
	// span is too small to be worth the extra cache entry.
	MidStableMessageCount              int    `json:"mid_stable_message_count,omitempty"`
	CacheComparatorPrefixHash          string `json:"cache_comparator_prefix_hash,omitempty"`
	CacheComparatorPrefixBytes         int    `json:"cache_comparator_prefix_bytes,omitempty"`
	CacheComparatorPrefixTokenEstimate int    `json:"cache_comparator_prefix_token_estimate,omitempty"`
	DecoratedProviderPrefixHash        string `json:"decorated_provider_prefix_hash,omitempty"`
}
