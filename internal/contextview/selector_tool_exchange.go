package contextview

import (
	"strings"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/userinput"
)

const toolExchangeDropReason = "tool_exchange:stripped"

// applyToolExchangePolicy strips bulky tool interactions from history-slot
// message fragments: tool-result fragments drop (except ask_user answers) and
// assistant fragments lose their non-ask_user tool-call parts while keeping
// visible text. Dropped fragments and content edits are reported so the
// manifest can explain both.
func applyToolExchangePolicy(frags []contextfrag.ContextFrag, policy *contextfrag.ToolExchangePolicy) (kept, dropped []contextfrag.ContextFrag, edits []contextfrag.ContextEditTrace) {
	if policy == nil {
		return frags, nil, nil
	}
	if policy.MinMessages > 0 && countMessageFrags(frags) <= policy.MinMessages {
		return frags, nil, nil
	}
	kept = make([]contextfrag.ContextFrag, 0, len(frags))
	for _, frag := range frags {
		msg := discussFragMessage(frag)
		if msg == nil || frag.Slot != contextfrag.SlotHistory {
			kept = append(kept, frag)
			continue
		}
		switch msg.Role {
		case sdk.MessageRoleTool:
			results := askUserToolResults(msg.Content)
			if len(results) == 0 {
				dropped = append(dropped, frag)
				continue
			}
			if len(results) != len(msg.Content) {
				frag = rebuildMessageFrag(frag, sdk.Message{Role: sdk.MessageRoleTool, Content: results})
				edits = append(edits, toolExchangeEdit(frag))
			}
			kept = append(kept, frag)
		case sdk.MessageRoleAssistant:
			parts, changed := stripAssistantToolParts(msg.Content)
			if !changed {
				kept = append(kept, frag)
				continue
			}
			if len(parts) == 0 {
				dropped = append(dropped, frag)
				continue
			}
			frag = rebuildMessageFrag(frag, sdk.Message{Role: sdk.MessageRoleAssistant, Content: parts})
			edits = append(edits, toolExchangeEdit(frag))
			kept = append(kept, frag)
		default:
			kept = append(kept, frag)
		}
	}
	return kept, dropped, edits
}

func countMessageFrags(frags []contextfrag.ContextFrag) int {
	count := 0
	for _, frag := range frags {
		if discussFragMessage(frag) != nil {
			count++
		}
	}
	return count
}

func askUserToolResults(parts []sdk.MessagePart) []sdk.MessagePart {
	kept := make([]sdk.MessagePart, 0, len(parts))
	for _, part := range parts {
		result, ok := part.(sdk.ToolResultPart)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(result.ToolName), userinput.ToolNameAskUser) {
			kept = append(kept, result)
		}
	}
	return kept
}

// stripAssistantToolParts keeps visible text and ask_user tool calls, and
// removes other tool calls, tool results and reasoning.
func stripAssistantToolParts(parts []sdk.MessagePart) ([]sdk.MessagePart, bool) {
	kept := make([]sdk.MessagePart, 0, len(parts))
	changed := false
	for _, part := range parts {
		switch typed := part.(type) {
		case sdk.ToolCallPart:
			if strings.EqualFold(strings.TrimSpace(typed.ToolName), userinput.ToolNameAskUser) {
				kept = append(kept, typed)
				continue
			}
			changed = true
		case sdk.ToolResultPart, sdk.ReasoningPart:
			changed = true
		case sdk.TextPart:
			if strings.TrimSpace(typed.Text) != "" {
				kept = append(kept, typed)
				continue
			}
			changed = true
		default:
			kept = append(kept, part)
		}
	}
	return kept, changed
}

func rebuildMessageFrag(frag contextfrag.ContextFrag, msg sdk.Message) contextfrag.ContextFrag {
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:            frag.ID,
		Message:       msg,
		Kind:          frag.Kind,
		Slot:          frag.Slot,
		Priority:      frag.Priority,
		CacheClass:    frag.CacheClass,
		Trust:         frag.Trust,
		Scope:         frag.Scope,
		Source:        frag.Provenance.Source,
		SourceID:      frag.Provenance.SourceID,
		Collector:     frag.Provenance.Collector,
		Index:         frag.Provenance.Index,
		Budget:        frag.Budget,
		TokenEstimate: frag.TokenEstimate,
	})
}

func toolExchangeEdit(frag contextfrag.ContextFrag) contextfrag.ContextEditTrace {
	ref := frag.Ref
	if err := contextfrag.ValidateContextRef(ref); err != nil {
		ref = contextfrag.WithContextRef(frag, ref).Ref
	}
	return contextfrag.ContextEditTrace{
		EditID: "tool_exchange.strip." + frag.ID,
		Op:     contextfrag.EditReplace,
		Slot:   frag.Slot,
		Refs:   []contextfrag.ContextRef{ref},
	}
}

func appendToolExchangeDrops(result SelectionResult, dropped []contextfrag.ContextFrag, edits []contextfrag.ContextEditTrace) SelectionResult {
	result.Edited = append(result.Edited, edits...)
	if len(dropped) == 0 {
		return result
	}
	for _, frag := range dropped {
		result.Dropped = append(result.Dropped, frag)
		result.Summary.DropReasons = append(result.Summary.DropReasons, DropRecord{
			FragID: frag.ID,
			Ref:    frag.Ref,
			Reason: toolExchangeDropReason,
		})
	}
	result.Summary.TotalCollected += len(dropped)
	result.Summary.TotalDropped += len(dropped)
	return result
}
