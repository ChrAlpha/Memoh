package native

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// requestUsage binds actual input usage to a complete request prefix. It
// retains hashes and counts only, never prompts, tool results or credentials.
// A process restart discards this hint; correctness still comes from durable
// checkpoints and bounded provider-overflow recovery.
type requestUsage struct {
	identity [32]byte
	envelope [32]byte
	prefix   [32]byte
	messages int
	estimate int
	actual   int
}

type usageScope struct{ bot, thread string }

// usageCache carries a bounded set of measurements across turns. Every run
// owns its own meter; concurrent sessions never share mutable request state.
type usageCache struct {
	mu     sync.Mutex
	values map[usageScope]requestUsage
}

type requestUsageMeter struct {
	identity [32]byte
	baseline requestUsage
}

func (a *Agent) withUsageMeter(cfg RunConfig) RunConfig {
	identity := []any{cfg.CurrentModelUUID, cfg.CurrentModelProviderID, cfg.ModelEndpoint, cfg.ChatCompletionsCompat}
	if cfg.Model != nil {
		identity = append(identity, cfg.Model.ID, cfg.Model.Reasoning)
		if cfg.Model.Provider != nil {
			identity = append(identity, cfg.Model.Provider.Name())
		}
	}
	// Direct runtime users without a catalog identity reuse only the same
	// model instance, since its provider endpoint is otherwise opaque.
	if cfg.CurrentModelUUID == "" {
		identity = append(identity, fmt.Sprintf("%p", cfg.Model))
	}
	raw, _ := json.Marshal(identity)
	meter := &requestUsageMeter{identity: sha256.Sum256(raw)}
	if cfg.Identity.SessionID != "" {
		a.usage.mu.Lock()
		meter.baseline = a.usage.values[usageScope{cfg.Identity.BotID, cfg.Identity.SessionID}]
		a.usage.mu.Unlock()
	}
	cfg.requestUsage = meter
	return cfg
}

func usageEnvelope(req sdk.Request) ([32]byte, bool) {
	req.Messages = nil
	req.Tools = slices.Clone(req.Tools)
	for i := range req.Tools {
		req.Tools[i].CacheControl = nil
	}
	raw, err := json.Marshal(req)
	return sha256.Sum256(raw), err == nil
}

func usagePrefix(messages []sdk.Message) ([32]byte, bool) {
	messages = cloneProviderMessages(messages)
	clearProviderCacheControls(messages)
	for i := range messages {
		messages[i].Usage = nil
	}
	raw, err := json.Marshal(messages)
	return sha256.Sum256(raw), err == nil
}

func requestInputTokens(cfg RunConfig, req sdk.Request) int {
	estimate := contextfrag.ProviderEnvelopeTokens(req.System, req.Messages, req.Tools)
	meter := cfg.requestUsage
	if meter == nil {
		return estimate
	}
	base := meter.baseline
	if base.actual <= 0 || base.identity != meter.identity || len(req.Messages) < base.messages {
		return estimate
	}
	envelope, ok := usageEnvelope(req)
	if !ok || envelope != base.envelope {
		return estimate
	}
	prefix, ok := usagePrefix(req.Messages[:base.messages])
	if !ok || prefix != base.prefix {
		return estimate
	}
	// SDK InputTokens already includes cache reads/writes. Reasoning and
	// generated output are not additional input until represented in the
	// appended messages. Leave 5% uncertainty beside the output reserve.
	measured := base.actual + (base.actual+19)/20 + max(0, estimate-base.estimate)
	return max(estimate, measured)
}

func (a *Agent) observeRequestUsage(ctx context.Context, cfg RunConfig, req sdk.Request, usage sdk.Usage) {
	if cfg.requestUsage == nil {
		return
	}
	measurement := requestUsage{identity: cfg.requestUsage.identity}
	if usage.InputTokens > 0 {
		var envelopeOK, prefixOK bool
		measurement.envelope, envelopeOK = usageEnvelope(req)
		measurement.prefix, prefixOK = usagePrefix(req.Messages)
		if envelopeOK && prefixOK {
			measurement.messages = len(req.Messages)
			measurement.estimate = contextfrag.ProviderEnvelopeTokens(req.System, req.Messages, req.Tools)
			measurement.actual = usage.InputTokens
		}
	}
	// Missing usage invalidates a preceding measurement rather than binding
	// its count to a different request.
	cfg.requestUsage.baseline = measurement
	if cfg.Identity.SessionID != "" {
		a.usage.mu.Lock()
		if a.usage.values == nil {
			a.usage.values = make(map[usageScope]requestUsage)
		}
		key := usageScope{cfg.Identity.BotID, cfg.Identity.SessionID}
		if _, exists := a.usage.values[key]; !exists && len(a.usage.values) >= 256 {
			for old := range a.usage.values {
				delete(a.usage.values, old)
				break
			}
		}
		a.usage.values[key] = measurement
		a.usage.mu.Unlock()
	}
	if measurement.actual > 0 {
		a.logger.InfoContext(ctx, "provider input usage measured", slog.String("run_id", cfg.RunID),
			slog.Int("estimated_input_tokens", measurement.estimate), slog.Int("actual_input_tokens", measurement.actual),
			slog.Int("input_allowance", providerInputAllowance(cfg)), slog.Int("output_reserve", cfg.GenerationLimits().MaxOutputTokens))
	}
}
