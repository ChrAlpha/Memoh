package contextview

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
			Scope:         req.Scope,
			Source:        contextfrag.SourceRunConfig,
			Collector:     historyMessagesCollectorName,
			Index:         i,
			Budget:        budget,
			TokenEstimate: estimate,
		}))
	}
	if cfg.RepairToolClosures {
		frags = repairToolClosureFrags(frags, req.Scope)
	}
	return frags, nil
}

const (
	toolClosureRepairSource = "context_repair"
	// syntheticToolClosureText mirrors the legacy resolver repair wording so
	// the model sees the same interruption notice.
	syntheticToolClosureText = "tool execution interrupted before a response was recorded"
)

// repairToolClosureFrags walks history message fragments and enforces tool
// closure integrity: every assistant tool call is answered before the next
// non-tool message (synthesizing an interrupted-result when missing) and tool
// results without a pending call are removed.
func repairToolClosureFrags(frags []contextfrag.ContextFrag, scope contextfrag.Scope) []contextfrag.ContextFrag {
	repaired := make([]contextfrag.ContextFrag, 0, len(frags))
	pendingOrder := make([]string, 0, 2)
	pendingNames := make(map[string]string, 2)
	syntheticIndex := 0

	flush := func() {
		for _, callID := range pendingOrder {
			repaired = append(repaired, syntheticToolClosureFrag(callID, pendingNames[callID], syntheticIndex, scope))
			syntheticIndex++
			delete(pendingNames, callID)
		}
		pendingOrder = pendingOrder[:0]
	}

	for _, frag := range frags {
		msg := discussFragMessage(frag)
		if msg == nil {
			flush()
			repaired = append(repaired, frag)
			continue
		}
		switch msg.Role {
		case sdk.MessageRoleAssistant:
			flush()
			repaired = append(repaired, frag)
			for _, part := range msg.Content {
				call, ok := part.(sdk.ToolCallPart)
				if !ok {
					continue
				}
				callID := strings.TrimSpace(call.ToolCallID)
				if callID == "" {
					continue
				}
				if _, exists := pendingNames[callID]; exists {
					continue
				}
				pendingNames[callID] = strings.TrimSpace(call.ToolName)
				pendingOrder = append(pendingOrder, callID)
			}
		case sdk.MessageRoleTool:
			kept := make([]sdk.MessagePart, 0, len(msg.Content))
			for _, part := range msg.Content {
				result, ok := part.(sdk.ToolResultPart)
				if !ok {
					continue
				}
				callID := strings.TrimSpace(result.ToolCallID)
				if _, pending := pendingNames[callID]; !pending {
					continue
				}
				kept = append(kept, result)
				delete(pendingNames, callID)
				for i, id := range pendingOrder {
					if id == callID {
						pendingOrder = append(pendingOrder[:i], pendingOrder[i+1:]...)
						break
					}
				}
			}
			if len(kept) == 0 {
				continue
			}
			if len(kept) != len(msg.Content) {
				frag = rebuildMessageFrag(frag, sdk.Message{Role: sdk.MessageRoleTool, Content: kept})
			}
			repaired = append(repaired, frag)
		default:
			flush()
			repaired = append(repaired, frag)
		}
	}
	flush()
	return repaired
}

func syntheticToolClosureFrag(callID, toolName string, index int, scope contextfrag.Scope) contextfrag.ContextFrag {
	msg := sdk.Message{
		Role: sdk.MessageRoleTool,
		Content: []sdk.MessagePart{sdk.ToolResultPart{
			ToolCallID: callID,
			ToolName:   toolName,
			Result:     syntheticToolClosureText,
			IsError:    true,
		}},
	}
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:         fmt.Sprintf("history.tool_closure.%03d", index),
		Message:    msg,
		Kind:       contextfrag.KindConversationEvent,
		Slot:       contextfrag.SlotHistory,
		Priority:   contextfrag.PriorityForMessage(msg),
		CacheClass: contextfrag.CacheNever,
		Trust:      contextfrag.TrustWorkspace,
		Scope:      scope,
		Source:     toolClosureRepairSource,
		SourceID:   "tool_closure." + callID,
		Collector:  historyMessagesCollectorName,
		Index:      index,
	})
}

func historyMessagesConfig(config any) (HistoryMessagesConfig, error) {
	if config == nil {
		return HistoryMessagesConfig{}, nil
	}
	switch cfg := config.(type) {
	case HistoryMessagesConfig:
		return cfg, nil
	case *HistoryMessagesConfig:
		if cfg == nil {
			return HistoryMessagesConfig{}, nil
		}
		return *cfg, nil
	default:
		return HistoryMessagesConfig{}, errors.New("history_messages config must be HistoryMessagesConfig")
	}
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
