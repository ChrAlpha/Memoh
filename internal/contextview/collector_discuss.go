package contextview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/pipeline"
)

const (
	discussContextCollectorName = "discuss_context"
	discussContextSource        = "pipeline_discuss"
)

type DiscussContextConfig struct {
	RC             pipeline.RenderedContext
	TRs            []pipeline.TurnResponseEntry
	CompactSummary string
}

type DiscussContextCollector struct{}

func (*DiscussContextCollector) Name() string {
	return discussContextCollectorName
}

func (*DiscussContextCollector) Collect(_ context.Context, req CollectRequest) ([]contextfrag.ContextFrag, error) {
	cfg, err := discussContextConfig(req.Config)
	if err != nil {
		return nil, err
	}

	messages := pipeline.MergeContext(cfg.RC, cfg.TRs)
	if len(messages) == 0 && cfg.CompactSummary == "" {
		return nil, nil
	}

	frags := make([]contextfrag.ContextFrag, 0, len(messages)+1)
	if cfg.CompactSummary != "" {
		frags = append(frags, contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:         "discuss.summary",
			Message:    sdk.UserMessage("[Conversation summary]\n" + cfg.CompactSummary),
			Kind:       contextfrag.KindConversationSummary,
			Slot:       contextfrag.SlotBeforeHistory,
			Priority:   10,
			CacheClass: contextfrag.CacheDynamic,
			Trust:      contextfrag.TrustSystem,
			Scope:      req.Scope,
			Source:     discussContextSource,
			SourceID:   "summary",
			Collector:  discussContextCollectorName,
			Index:      -1,
			Budget:     contextfrag.BudgetPolicy{Overflow: contextfrag.OverflowKeep},
		}))
	}

	for i, msg := range messages {
		sdkMsg := discussContextMessageToSDK(msg)
		frags = append(frags, contextfrag.MessageFrag(contextfrag.MessageFragInput{
			ID:         fmt.Sprintf("discuss.%03d", i),
			Message:    sdkMsg,
			Kind:       contextfrag.KindConversationEvent,
			Slot:       contextfrag.SlotHistory,
			Priority:   contextfrag.PriorityForMessage(sdkMsg),
			CacheClass: contextfrag.CacheNever,
			Trust:      trustForDiscussRole(msg.Role),
			Scope:      req.Scope,
			Source:     discussContextSource,
			SourceID:   fmt.Sprintf("message.%03d", i),
			Collector:  discussContextCollectorName,
			Index:      i,
		}))
	}
	return frags, nil
}

func discussContextConfig(config any) (DiscussContextConfig, error) {
	if config == nil {
		return DiscussContextConfig{}, nil
	}
	switch cfg := config.(type) {
	case DiscussContextConfig:
		return cfg, nil
	case *DiscussContextConfig:
		if cfg == nil {
			return DiscussContextConfig{}, nil
		}
		return *cfg, nil
	default:
		return DiscussContextConfig{}, fmt.Errorf("discuss_context config must be DiscussContextConfig")
	}
}

func discussContextMessageToSDK(m pipeline.ContextMessage) sdk.Message {
	if len(m.RawContent) > 0 {
		raw, err := json.Marshal(struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}{
			Role:    m.Role,
			Content: m.RawContent,
		})
		if err == nil {
			var msg sdk.Message
			if json.Unmarshal(raw, &msg) == nil {
				return msg
			}
		}
	}
	switch m.Role {
	case "user":
		return sdk.UserMessage(m.Content)
	case "assistant":
		return sdk.AssistantMessage(m.Content)
	case "tool":
		return sdk.UserMessage(m.Content)
	default:
		return sdk.UserMessage(m.Content)
	}
}

func trustForDiscussRole(role string) contextfrag.TrustLevel {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant", "tool":
		return contextfrag.TrustWorkspace
	default:
		return contextfrag.TrustExternal
	}
}
