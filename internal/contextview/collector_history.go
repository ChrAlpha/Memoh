package contextview

import (
	"context"
	"fmt"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
)

const historyMessagesCollectorName = "history_messages"

type HistoryMessagesConfig struct {
	Messages []sdk.Message
	// TokenEstimates carries per-message context token estimates computed at
	// the source (parallel to Messages; may be shorter). Entries beyond the
	// array fall back to renderer-side estimation.
	TokenEstimates []int
	// TrimmablePrefix marks how many leading messages are droppable history.
	// Messages at or beyond this index (memory, current request) are pinned.
	// Zero means nothing is trimmable.
	TrimmablePrefix int
	// RepairToolClosures closes dangling assistant tool calls with synthetic
	// tool results and drops orphan tool results, so the provider never sees
	// a broken closure.
	RepairToolClosures bool
}

type HistoryMessagesCollector struct{}

func (*HistoryMessagesCollector) Name() string {
	return historyMessagesCollectorName
}

func (*HistoryMessagesCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := historyMessagesConfig(req.Config)
	if err != nil {
		return nil, err
	}
	if len(cfg.Messages) == 0 {
		return nil, nil
	}

	// History messages carry no per-message attention data on this path; the
	// request's own attention must not color them, or budget drop histograms
	// would report the current turn's attention for every history drop.
	scope := req.Scope
	scope.Attention = nil

	frags := make([]contextfrag.ContextFrag, 0, len(cfg.Messages))
	for i, msg := range cfg.Messages {
		estimate := 0
		if i < len(cfg.TokenEstimates) {
			estimate = cfg.TokenEstimates[i]
		}
		budget := contextfrag.BudgetPolicy{}
		if i >= cfg.TrimmablePrefix {
			budget.Overflow = contextfrag.OverflowKeep
		}
		frags = append(frags, contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:            fmt.Sprintf("message.%03d", i),
			Message:       msg,
			Kind:          kindForSDKMessage(msg),
			Slot:          contextfrag.SlotHistory,
			Priority:      contextfrag.PriorityForMessage(msg),
			CacheClass:    cacheForSDKMessage(msg),
			Trust:         trustForSDKMessage(msg),
			Scope:         scope,
			Source:        contextfrag.SourceRunConfig,
			Collector:     historyMessagesCollectorName,
			Index:         i,
			Budget:        budget,
			TokenEstimate: estimate,
		}))
	}
	if cfg.RepairToolClosures {
		frags = contextfrag.RepairToolClosureFrags(frags, scope, historyMessagesCollectorName)
	}
	return frags, nil
}

func historyMessagesConfig(config any) (HistoryMessagesConfig, error) {
	return collectorConfig[HistoryMessagesConfig](config, "history_messages config must be HistoryMessagesConfig")
}

func kindForSDKMessage(msg sdk.Message) contextfrag.Kind {
	switch msg.Role {
	case sdk.MessageRoleSystem:
		return contextfrag.KindSystemPolicy
	case sdk.MessageRoleUser, sdk.MessageRoleAssistant, sdk.MessageRoleTool:
		return contextfrag.KindConversationEvent
	default:
		return contextfrag.KindConversationEvent
	}
}

func cacheForSDKMessage(msg sdk.Message) contextfrag.CacheClass {
	switch msg.Role {
	case sdk.MessageRoleSystem:
		return contextfrag.CacheDynamic
	case sdk.MessageRoleUser, sdk.MessageRoleAssistant, sdk.MessageRoleTool:
		return contextfrag.CacheNever
	default:
		return contextfrag.CacheNever
	}
}

func trustForSDKMessage(msg sdk.Message) contextfrag.TrustLevel {
	switch msg.Role {
	case sdk.MessageRoleSystem:
		return contextfrag.TrustSystem
	case sdk.MessageRoleAssistant, sdk.MessageRoleTool:
		return contextfrag.TrustWorkspace
	case sdk.MessageRoleUser:
		return contextfrag.TrustExternal
	default:
		return contextfrag.TrustExternal
	}
}
