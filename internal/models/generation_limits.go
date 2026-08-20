package models

const (
	// DefaultOutputReserveTokens is the completion allowance reserved for a
	// turn when the provider's own cap is not mirrored exactly.
	DefaultOutputReserveTokens = 8192
	// DefaultReasoningOutputReserveTokens is the allowance when thinking is
	// active; reasoning output shares the completion cap on every provider.
	DefaultReasoningOutputReserveTokens = 32000

	anthropicDefaultMaxTokens          = 4096
	anthropicDefaultReasoningMaxTokens = 32000
)

const (
	GenerationLimitsProviderDefault = "provider_default"
	GenerationLimitsPolicyCap       = "policy_cap"
	GenerationLimitsEstimated       = "estimated"
	GenerationLimitsProviderIgnores = "provider_ignores"
	GenerationLimitsWindowClamped   = "window_clamped"
)

// GenerationLimits is the single authority for one turn's output allowance:
// the same MaxOutputTokens reserves window space in the context budget plan
// and, when Enforced, is sent to the provider as max_tokens. Unenforced
// limits are Memoh's reserve only; the provider keeps its own default because
// the model's real cap is unknown and an explicit value could be rejected.
type GenerationLimits struct {
	MaxOutputTokens int
	Enforced        bool
	Resolution      string
}

// ResolveGenerationLimits derives the output allowance from the client type
// and the resolved thinking decision. Anthropic mirrors the SDK's own
// defaults exactly (answer allowance, plus the thinking budget for legacy
// models, or the reasoning-aware default for adaptive thinking), so the
// reserved and requested values cannot diverge; OpenAI Responses models all
// accept the policy caps; other clients are estimated without enforcement.
func ResolveGenerationLimits(clientType ClientType, reasoning *ReasoningConfig, contextWindow int) GenerationLimits {
	active := reasoning != nil && reasoning.Active
	limits := GenerationLimits{MaxOutputTokens: DefaultOutputReserveTokens, Resolution: GenerationLimitsEstimated}
	if active {
		limits.MaxOutputTokens = DefaultReasoningOutputReserveTokens
	}
	legacyThinking := false
	switch clientType {
	case ClientTypeAnthropicMessages:
		limits.Enforced = true
		limits.Resolution = GenerationLimitsProviderDefault
		switch {
		case active && reasoning.Adaptive:
			limits.MaxOutputTokens = anthropicDefaultReasoningMaxTokens
		case active:
			legacyThinking = true
			limits.MaxOutputTokens = anthropicDefaultMaxTokens + legacyAnthropicBudgetFor(reasoning.Effort)
		default:
			limits.MaxOutputTokens = anthropicDefaultMaxTokens
		}
	case ClientTypeOpenAIResponses:
		limits.Enforced = true
		limits.Resolution = GenerationLimitsPolicyCap
	case ClientTypeOpenAICodex:
		limits.Resolution = GenerationLimitsProviderIgnores
	}
	if contextWindow > 0 && !legacyThinking && limits.MaxOutputTokens > contextWindow/4 {
		limits.MaxOutputTokens = contextWindow / 4
		limits.Resolution = GenerationLimitsWindowClamped
	}
	return limits
}
