package contextview

import (
	"context"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

const hookContextCollectorName = "hook_context"

type HookContextConfig struct {
	// Text is the combined resolver-hook AppendContext text (before/after
	// prompt build) materialized by the resolver; the collector owns its
	// shape and placement in the prompt so it never touches the cacheable
	// system prefix.
	Text string
}

// HookContextCollector turns the materialized resolver-hook context into a
// fragment placed between the conversation history and the current request,
// keeping dynamic hook output out of the system prompt.
type HookContextCollector struct{}

func (*HookContextCollector) Name() string {
	return hookContextCollectorName
}

func (*HookContextCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := hookContextConfig(req.Config)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(cfg.Text)
	if text == "" {
		return nil, nil
	}
	msg := sdk.SystemMessage(text)
	return []contextfrag.ContextFrag{contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "hook_context.message",
		Message:    msg,
		Kind:       contextfrag.KindHookContext,
		Slot:       contextfrag.SlotAfterHistoryBeforeCurrent,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustSystem,
		Scope:      req.Scope,
		Source:     hookContextCollectorName,
		SourceID:   hookContextCollectorName,
		Collector:  hookContextCollectorName,
		Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})}, nil
}

func hookContextConfig(config any) (HookContextConfig, error) {
	return collectorConfig[HookContextConfig](config, "hook_context config must be HookContextConfig")
}
