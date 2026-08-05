package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"math/rand"
	"slices"
	"sort"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/contextview"
)

var s1Windows = []int{200_000, 128_000, 64_000, 32_000, 16_000, 12_000, 8_000}

type s1Inventory struct {
	Total       int      `json:"total"`
	Survived    int      `json:"survived"`
	Dropped     int      `json:"dropped"`
	SurvivedIDs []string `json:"survived_ids"`
	DroppedIDs  []string `json:"dropped_ids"`
}

type s1Record struct {
	Scenario                string      `json:"scenario"`
	Variant                 string      `json:"variant"`
	WindowTokens            int         `json:"window_tokens"`
	Estimator               string      `json:"estimator"`
	EstimatorSafetyPercent  int         `json:"estimator_safety_factor_percent"`
	OutputReserveTokens     int         `json:"output_reserve_tokens"`
	PayloadTokens           int         `json:"payload_tokens"`
	EnvelopeTokens          int         `json:"envelope_tokens"`
	PayloadBytes            int         `json:"payload_bytes"`
	FitsWindow              bool        `json:"fits_window"`
	OverflowTokens          int         `json:"overflow_tokens"`
	ProviderCallAllowed     bool        `json:"provider_call_allowed"`
	CompileError            string      `json:"compile_error,omitempty"`
	PayloadHash             string      `json:"payload_hash"`
	SystemItems             s1Inventory `json:"system_items"`
	RequiredSectionsIntact  bool        `json:"required_sections_intact"`
	MarkerPresent           bool        `json:"marker_present"`
	MarkerBytes             int         `json:"marker_bytes"`
	DropOrderCorrect        bool        `json:"drop_order_correct"`
	DropOrderIDs            []string    `json:"drop_order_ids,omitempty"`
	DistinctRecordHashCount int         `json:"distinct_record_hash_count"`
}

func runS1(fixture benchFixture) []s1Record {
	records := make([]s1Record, 0, len(s1Windows)*2)
	for _, variant := range []string{"legacy", "typed"} {
		for _, window := range s1Windows {
			seen := make(map[string]struct{}, 5)
			var first s1Record
			for repetition := range 5 {
				record := measureS1(fixture, variant, window)
				record.DistinctRecordHashCount = 0
				raw, err := marshalStable(record)
				if err != nil {
					panic(err)
				}
				sum := sha256.Sum256(raw)
				seen[hex.EncodeToString(sum[:])] = struct{}{}
				if repetition == 0 {
					first = record
				}
			}
			first.DistinctRecordHashCount = len(seen)
			records = append(records, first)
		}
	}
	return records
}

func measureS1(fixture benchFixture, variant string, window int) s1Record {
	reserve := min(contextview.DefaultOutputReserveTokens, window/4)
	payload := legacyPayload(fixture)
	providerCallAllowed := true
	compileError := ""
	survivedIDs := slices.Clone(fixture.systemIDs)
	var droppedIDs []string
	markerPresent := false
	markerBytes := 0
	dropOrderCorrect := true
	var dropOrderIDs []string
	if variant == "typed" {
		cfg, err := contextview.ProviderRunConfigApplier(nil)(context.Background(), typedConfig(fixture, fixture.sourceFrags, window))
		providerCallAllowed = err == nil
		if err != nil {
			compileError = budgetErrorLabel(err)
		}
		if err == nil {
			payload = providerPayload{System: cfg.System, Messages: cfg.Messages, Tools: fixture.tools}
		} else {
			audit := contextfrag.Render(cfg.ContextFrags)
			payload = providerPayload{System: audit.System, Messages: audit.Messages, Tools: fixture.tools}
		}
		survivedIDs, droppedIDs, markerPresent, markerBytes = s1TypedInventory(fixture, cfg.ContextManifest)
		dropOrderCorrect, dropOrderIDs = validateSystemDropOrder(fixture.systemFrags, cfg.ContextManifest)
	}
	hash, payloadBytes, _ := providerPayloadMetrics(payload)
	payloadTokens := providerEnvelopeTokens(payload)
	envelopeTokens := payloadTokens + reserve
	overflow := max(0, envelopeTokens-window)
	return s1Record{
		Scenario: "s1_granularity", Variant: variant, WindowTokens: window,
		Estimator: contextfrag.ProviderBudgetEstimator, EstimatorSafetyPercent: contextfrag.ProviderBudgetSafetyFactorPercent,
		OutputReserveTokens: reserve, PayloadTokens: payloadTokens, EnvelopeTokens: envelopeTokens, PayloadBytes: payloadBytes,
		FitsWindow: providerCallAllowed && overflow == 0, OverflowTokens: overflow, ProviderCallAllowed: providerCallAllowed,
		CompileError: compileError, PayloadHash: hash,
		SystemItems: s1Inventory{
			Total: len(fixture.systemIDs), Survived: len(survivedIDs), Dropped: len(droppedIDs),
			SurvivedIDs: survivedIDs, DroppedIDs: droppedIDs,
		},
		RequiredSectionsIntact: containsAll(survivedIDs, fixture.requiredIDs) && containsMessage(payload.Messages, fixture.messages[len(fixture.messages)-1]), MarkerPresent: markerPresent,
		MarkerBytes: markerBytes, DropOrderCorrect: dropOrderCorrect, DropOrderIDs: dropOrderIDs,
	}
}

func s1TypedInventory(fixture benchFixture, manifest contextfrag.Manifest) ([]string, []string, bool, int) {
	source := make(map[string]bool, len(fixture.systemIDs))
	for _, id := range fixture.systemIDs {
		source[id] = true
	}
	selected := make(map[string]bool, len(fixture.systemIDs))
	markerPresent := false
	markerBytes := 0
	for _, item := range manifest.Items {
		if source[item.ID] {
			selected[item.ID] = true
		}
		if item.ID == "system.budget_notice" {
			markerPresent = true
			markerBytes = item.TextBytes
		}
	}
	survived := make([]string, 0, len(selected))
	dropped := make([]string, 0, len(source)-len(selected))
	for _, id := range fixture.systemIDs {
		if selected[id] {
			survived = append(survived, id)
		} else {
			dropped = append(dropped, id)
		}
	}
	return survived, dropped, markerPresent, markerBytes
}

func validateSystemDropOrder(source []contextfrag.ContextFrag, manifest contextfrag.Manifest) (bool, []string) {
	byID := make(map[string]contextfrag.ContextFrag, len(source))
	selected := make(map[string]bool, len(manifest.Items))
	for _, item := range manifest.Items {
		selected[item.ID] = true
	}
	var expected []contextfrag.ContextFrag
	droppedCandidates := 0
	for _, frag := range source {
		byID[frag.ID] = frag
		if (frag.RetentionTier == contextfrag.RetentionOptional || frag.RetentionTier == contextfrag.RetentionPreferred) && !strings.HasSuffix(frag.ID, ".header") {
			expected = append(expected, frag)
			if !selected[frag.ID] {
				droppedCandidates++
			}
		}
	}
	sort.SliceStable(expected, func(i, j int) bool {
		left, right := expected[i], expected[j]
		if left.RetentionTier != right.RetentionTier {
			return left.RetentionTier == contextfrag.RetentionOptional
		}
		if left.DropPriority != right.DropPriority {
			return left.DropPriority > right.DropPriority
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		return left.ID < right.ID
	})
	actual := make([]string, 0)
	for _, edit := range manifest.EditTrace {
		id := strings.TrimPrefix(edit.EditID, "selection.drop.")
		frag, ok := byID[id]
		if !ok || strings.HasSuffix(frag.ID, ".header") {
			continue
		}
		actual = append(actual, id)
	}
	if len(actual) != droppedCandidates || len(actual) > len(expected) {
		return false, actual
	}
	for i, id := range actual {
		if id != expected[i].ID {
			return false, actual
		}
	}
	return true, actual
}

func budgetErrorLabel(err error) string {
	switch {
	case errors.Is(err, contextfrag.ErrProtectedContextOverflow):
		return "protected_context_overflow"
	case errors.Is(err, contextfrag.ErrBudgetUnsatisfied):
		return "budget_unsatisfied"
	default:
		return err.Error()
	}
}

func containsAll(values, required []string) bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	for _, value := range required {
		if !set[value] {
			return false
		}
	}
	return true
}

func containsMessage(messages []sdk.Message, required sdk.Message) bool {
	for _, message := range messages {
		if equalMessages(message, required) {
			return true
		}
	}
	return false
}

type s2Record struct {
	Scenario                  string  `json:"scenario"`
	Variant                   string  `json:"variant"`
	Turn                      int     `json:"turn"`
	SystemHash                string  `json:"system_hash"`
	SystemBytes               int     `json:"system_bytes"`
	PayloadHash               string  `json:"payload_hash"`
	PayloadBytes              int     `json:"payload_bytes"`
	PayloadTokens             int     `json:"payload_tokens"`
	LongestCommonPrefixTokens int     `json:"longest_common_prefix_tokens"`
	SimulatedCacheReadTokens  int     `json:"simulated_cache_read_tokens"`
	SimulatedCacheHitRatio    float64 `json:"simulated_cache_hit_ratio"`
	FirstDivergenceSegment    string  `json:"first_divergence_segment"`
	StablePrefixHash          string  `json:"stable_prefix_hash,omitempty"`
	StableMessageCount        int     `json:"stable_message_count,omitempty"`
}

type s2TurnInput struct {
	hook      string
	memory    string
	user      string
	assistant string
}

type s2VariantState struct {
	payload providerPayload
	raw     []byte
}

func runS2(fixture benchFixture) []s2Record {
	turns := buildS2Turns()
	variants := []string{"legacy", "legacy-hooksys", "typed"}
	previous := make(map[string]s2VariantState, len(variants))
	history := make([]sdk.Message, 0, len(turns)*2)
	records := make([]s2Record, 0, len(turns)*len(variants))
	for turnIndex, input := range turns {
		for _, variant := range variants {
			payload, cachePlan := s2Payload(fixture, history, input, variant)
			raw := rawProviderPayload(payload)
			hash, payloadBytes, payloadTokens := providerPayloadMetrics(payload)
			lcpBytes := 0
			divergence := "initial"
			if prior, ok := previous[variant]; ok {
				lcpBytes = longestCommonPrefix(prior.raw, raw)
				divergence = firstDivergenceSegment(prior.payload, payload)
			}
			lcpTokens := contextfrag.ProviderBudgetTokensFromBytes(lcpBytes)
			ratio := 0.0
			if payloadTokens > 0 {
				ratio = math.Min(1, float64(lcpTokens)/float64(payloadTokens))
			}
			records = append(records, s2Record{
				Scenario: "s2_prefix_stability", Variant: variant, Turn: turnIndex + 1,
				SystemHash: hashString(payload.System), SystemBytes: len(payload.System), PayloadHash: hash,
				PayloadBytes: payloadBytes, PayloadTokens: payloadTokens, LongestCommonPrefixTokens: lcpTokens,
				SimulatedCacheReadTokens: lcpTokens, SimulatedCacheHitRatio: ratio, FirstDivergenceSegment: divergence,
				StablePrefixHash: cachePlan.StablePrefixHash, StableMessageCount: cachePlan.StableMessageCount,
			})
			previous[variant] = s2VariantState{payload: payload, raw: raw}
		}
		history = append(history, sdk.UserMessage(input.user), sdk.AssistantMessage(input.assistant))
	}
	return records
}

func buildS2Turns() []s2TurnInput {
	rng := rand.New(rand.NewSource(fixtureSeed + 2)) //nolint:gosec // a fixed seed is the benchmark contract
	turns := make([]s2TurnInput, 60)
	for i := range turns {
		turns[i] = s2TurnInput{
			hook:      "[Hook Context turn=" + threeDigits(i+1) + "]\n" + seededText(rng, 220+rng.Intn(581), i%4 == 0),
			memory:    "[Memory recall turn=" + threeDigits(i+1) + "]\n" + seededText(rng, 380+rng.Intn(921), i%5 == 0),
			user:      "[History user turn=" + threeDigits(i+1) + "] " + seededText(rng, 160+rng.Intn(421), i%6 == 0),
			assistant: "[History assistant turn=" + threeDigits(i+1) + "] " + seededText(rng, 240+rng.Intn(701), i%7 == 0),
		}
	}
	return turns
}

func s2Payload(fixture benchFixture, history []sdk.Message, input s2TurnInput, variant string) (providerPayload, contextfrag.CachePlan) {
	baseSystem := flattenSystem(fixture.systemFrags)
	current := sdk.UserMessage("[Current query] " + input.user)
	memory := sdk.UserMessage(input.memory)
	hookMessage := sdk.UserMessage(input.hook)
	switch variant {
	case "legacy":
		messages := append(cloneMessages(history), memory, hookMessage, current)
		return providerPayload{System: baseSystem, Messages: messages}, contextfrag.CachePlan{}
	case "legacy-hooksys":
		// This is the explicitly named true-upstream hook-authority emulation,
		// separate from the tree's byte-equivalent legacy assembly baseline.
		messages := append(cloneMessages(history), memory, current)
		return providerPayload{System: baseSystem + "\n\n" + input.hook, Messages: messages}, contextfrag.CachePlan{}
	case "typed":
		source := slices.Clone(fixture.systemFrags)
		hookFrags, err := (&contextview.HookContextCollector{}).Collect(context.Background(), contextview.CollectRequest{
			Scope: contextfrag.Scope{BotID: "contextbench"}, Intent: contextfrag.IntentRunConfigPreProvider,
			Config: contextview.HookContextConfig{Text: input.hook},
		})
		if err != nil {
			panic(err)
		}
		source = append(source, hookFrags...)
		for i, message := range history {
			trust := contextfrag.TrustExternal
			if message.Role == sdk.MessageRoleAssistant {
				trust = contextfrag.TrustWorkspace
			}
			source = append(source, estimatedMessageFrag("message."+threeDigits(i), message, contextfrag.KindConversationEvent, contextfrag.SlotHistory, trust, i))
		}
		memoryFrags, err := (&contextview.MemoryContextCollector{}).Collect(context.Background(), contextview.CollectRequest{
			Scope: contextfrag.Scope{BotID: "contextbench"}, Intent: contextfrag.IntentRunConfigPreProvider,
			Config: contextview.MemoryContextConfig{Text: input.memory, Index: len(history)},
		})
		if err != nil {
			panic(err)
		}
		for i := range memoryFrags {
			memoryFrags[i].TokenEstimate = contextfrag.ResolveProviderBudgetFragTokens(memoryFrags[i])
		}
		source = append(source, memoryFrags...)
		currentFrag := estimatedMessageFrag("message.current", current, contextfrag.KindCurrentUserMessage, contextfrag.SlotCurrentUser, contextfrag.TrustUser, len(history)+1)
		currentFrag.CacheClass = contextfrag.CacheNever
		currentFrag.Budget.Overflow = contextfrag.OverflowKeep
		source = append(source, currentFrag)
		cfg, err := contextview.ProviderRunConfigApplier(nil)(context.Background(), typedConfig(fixture, source, 200_000))
		if err != nil {
			panic(err)
		}
		return providerPayload{System: cfg.System, Messages: cfg.Messages}, cfg.ContextCachePlan
	default:
		panic("unknown S2 variant: " + variant)
	}
}

func longestCommonPrefix(left, right []byte) int {
	limit := min(len(left), len(right))
	for i := range limit {
		if left[i] != right[i] {
			return i
		}
	}
	return limit
}

func firstDivergenceSegment(previous, current providerPayload) string {
	if previous.System != current.System {
		return "system.hook_context"
	}
	limit := min(len(previous.Messages), len(current.Messages))
	for i := range limit {
		if !equalMessages(previous.Messages[i], current.Messages[i]) {
			return classifyMessage(current.Messages[i])
		}
	}
	if len(current.Messages) > limit {
		return classifyMessage(current.Messages[limit])
	}
	if len(previous.Messages) != len(current.Messages) {
		return "messages.end"
	}
	return "none"
}

func classifyMessage(message sdk.Message) string {
	for _, part := range message.Content {
		text, ok := part.(sdk.TextPart)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(text.Text, "[History"):
			return "messages.history"
		case strings.HasPrefix(text.Text, "[Memory"):
			return "messages.memory_recall"
		case strings.HasPrefix(text.Text, "[Hook"):
			return "messages.hook_context"
		case strings.HasPrefix(text.Text, "[Current"):
			return "messages.current"
		}
	}
	return "messages.other"
}
