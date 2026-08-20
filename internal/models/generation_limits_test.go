package models

import "testing"

func TestResolveGenerationLimitsMirrorsAnthropicProviderDefaults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		reasoning *ReasoningConfig
		want      int
	}{
		{name: "no reasoning config", reasoning: nil, want: 4096},
		{name: "explicitly disabled", reasoning: &ReasoningConfig{Disabled: true}, want: 4096},
		{name: "adaptive", reasoning: &ReasoningConfig{Active: true, Adaptive: true, Effort: ReasoningEffortHigh}, want: 32000},
		{name: "legacy low", reasoning: &ReasoningConfig{Active: true, Effort: ReasoningEffortLow}, want: 4096 + 5000},
		{name: "legacy medium", reasoning: &ReasoningConfig{Active: true, Effort: ReasoningEffortMedium}, want: 4096 + 16000},
		{name: "legacy high", reasoning: &ReasoningConfig{Active: true, Effort: ReasoningEffortHigh}, want: 4096 + 50000},
		{name: "legacy unknown effort defaults to medium", reasoning: &ReasoningConfig{Active: true}, want: 4096 + 16000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveGenerationLimits(ClientTypeAnthropicMessages, tc.reasoning, 200_000)
			if got.MaxOutputTokens != tc.want {
				t.Fatalf("MaxOutputTokens = %d, want %d", got.MaxOutputTokens, tc.want)
			}
			if !got.Enforced {
				t.Fatal("Anthropic limits must be sent as max_tokens so the plan and the request share one value")
			}
			if got.Resolution != GenerationLimitsProviderDefault {
				t.Fatalf("Resolution = %q, want %q", got.Resolution, GenerationLimitsProviderDefault)
			}
		})
	}
}

func TestResolveGenerationLimitsCapsOpenAIResponses(t *testing.T) {
	t.Parallel()

	plain := ResolveGenerationLimits(ClientTypeOpenAIResponses, nil, 400_000)
	if plain.MaxOutputTokens != DefaultOutputReserveTokens || !plain.Enforced || plain.Resolution != GenerationLimitsPolicyCap {
		t.Fatalf("plain responses limits = %+v, want enforced policy cap %d", plain, DefaultOutputReserveTokens)
	}
	reasoning := ResolveGenerationLimits(ClientTypeOpenAIResponses, &ReasoningConfig{Active: true, Effort: ReasoningEffortHigh}, 400_000)
	if reasoning.MaxOutputTokens != DefaultReasoningOutputReserveTokens || !reasoning.Enforced {
		t.Fatalf("reasoning responses limits = %+v, want enforced policy cap %d", reasoning, DefaultReasoningOutputReserveTokens)
	}
}

func TestResolveGenerationLimitsEstimatesWithoutEnforcingForUnknownCatalogs(t *testing.T) {
	t.Parallel()

	for _, clientType := range []ClientType{ClientTypeOpenAICompletions, ClientTypeGoogleGenerativeAI, ClientTypeGitHubCopilot} {
		t.Run(string(clientType), func(t *testing.T) {
			t.Parallel()
			plain := ResolveGenerationLimits(clientType, nil, 128_000)
			if plain.MaxOutputTokens != DefaultOutputReserveTokens || plain.Enforced || plain.Resolution != GenerationLimitsEstimated {
				t.Fatalf("plain limits = %+v, want unenforced estimate %d", plain, DefaultOutputReserveTokens)
			}
			reasoning := ResolveGenerationLimits(clientType, &ReasoningConfig{Active: true}, 128_000)
			if reasoning.MaxOutputTokens != DefaultReasoningOutputReserveTokens || reasoning.Enforced {
				t.Fatalf("reasoning limits = %+v, want unenforced estimate %d", reasoning, DefaultReasoningOutputReserveTokens)
			}
		})
	}
}

func TestResolveGenerationLimitsNeverEnforcesOnCodex(t *testing.T) {
	t.Parallel()

	got := ResolveGenerationLimits(ClientTypeOpenAICodex, &ReasoningConfig{Active: true, Effort: ReasoningEffortHigh}, 400_000)
	if got.Enforced || got.Resolution != GenerationLimitsProviderIgnores || got.MaxOutputTokens != DefaultReasoningOutputReserveTokens {
		t.Fatalf("codex limits = %+v, want unenforced reserve %d with provider_ignores", got, DefaultReasoningOutputReserveTokens)
	}
}

func TestResolveGenerationLimitsClampsToAQuarterOfTheWindow(t *testing.T) {
	t.Parallel()

	got := ResolveGenerationLimits(ClientTypeOpenAICompletions, nil, 8_192)
	if got.MaxOutputTokens != 2_048 || got.Resolution != GenerationLimitsWindowClamped || got.Enforced {
		t.Fatalf("small-window limits = %+v, want 2048 window_clamped", got)
	}
	if got := ResolveGenerationLimits(ClientTypeOpenAICompletions, nil, 32_767); got.MaxOutputTokens != 8_191 {
		t.Fatalf("quarter-window clamp = %+v, want 8191", got)
	}
	if got := ResolveGenerationLimits(ClientTypeOpenAIResponses, &ReasoningConfig{Active: true}, 64_000); got.MaxOutputTokens != 16_000 || !got.Enforced {
		t.Fatalf("clamped enforced limits = %+v, want 16000 still enforced", got)
	}
	if got := ResolveGenerationLimits(ClientTypeOpenAICompletions, nil, 0); got.MaxOutputTokens != DefaultOutputReserveTokens {
		t.Fatalf("unknown window must not clamp: %+v", got)
	}
	legacy := ResolveGenerationLimits(ClientTypeAnthropicMessages, &ReasoningConfig{Active: true, Effort: ReasoningEffortHigh}, 64_000)
	if legacy.MaxOutputTokens != 4096+50000 {
		t.Fatalf("legacy thinking floor must survive the clamp (budget_tokens must stay below max_tokens): %+v", legacy)
	}
}
