package contextview

import (
	"context"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

const memoryContextCollectorName = "memory_context"

type MemoryContextConfig struct {
	// Text is the memory recall context materialized by the resolver
	// (provider search plus hook amendments); the collector owns its shape
	// and placement in the prompt.
	Text string
}

// MemoryContextCollector turns the materialized memory recall into a pinned
// fragment placed between the conversation history and the current request.
type MemoryContextCollector struct{}

func (*MemoryContextCollector) Name() string {
	return memoryContextCollectorName
}

func (*MemoryContextCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := memoryContextConfig(req.Config)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(cfg.Text)
	if text == "" {
		return nil, nil
	}
	msg := sdk.UserMessage(text)
	return []contextfrag.ContextFrag{contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         "memory.recall",
		Message:    msg,
		Kind:       contextfrag.KindMemoryRecall,
		Slot:       contextfrag.SlotHistory,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustSystem,
		Scope:      req.Scope,
		Source:     memoryContextCollectorName,
		SourceID:   "recall",
		Collector:  memoryContextCollectorName,
		Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
	})}, nil
}

func memoryContextConfig(config any) (MemoryContextConfig, error) {
	return collectorConfig[MemoryContextConfig](config, "memory_context config must be MemoryContextConfig")
}
