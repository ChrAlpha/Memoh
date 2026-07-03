package contextfrag

import sdk "github.com/memohai/twilight-ai/sdk"

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

// PriorityForMessage exposes the compile-path priority mapping to collectors.
func PriorityForMessage(msg sdk.Message) int {
	return priorityForMessage(msg)
}

// NewSummaryCoverage builds the coverage envelope linking a summary fragment
// to the fragments it covers.
func NewSummaryCoverage(summaryRef ContextRef, coveredRefs []ContextRef) SummaryCoverage {
	return SummaryCoverage{
		CoverageID:  "coverage:" + summaryRef.StableKey(),
		SummaryRef:  summaryRef,
		CoveredRefs: coveredRefs,
		Schema:      SchemaVersion{Name: SchemaSummaryCoverage, Version: CurrentSchemaVersion},
	}
}

// CachePlan is the placement-derived prompt cache shape handed to the
// provider call: the stable prefix identity plus how many leading rendered
// messages are cache-stable and may carry a cache breakpoint.
type CachePlan struct {
	StablePrefixHash                  string `json:"stable_prefix_hash,omitempty"`
	StableMessageCount                int    `json:"stable_message_count,omitempty"`
	RenderedStablePrefixHash          string `json:"rendered_prefix_hash,omitempty"`
	RenderedStablePrefixBytes         int    `json:"rendered_prefix_bytes,omitempty"`
	RenderedStablePrefixTokenEstimate int    `json:"rendered_prefix_token_estimate,omitempty"`
}

const HashScopeSourcePayload = "source_payload"
