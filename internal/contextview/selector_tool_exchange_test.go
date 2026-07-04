package contextview

import (
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/userinput"
)

func historyMessageFrag(id string, msg sdk.Message) contextfrag.ContextFrag {
	return contextfrag.MessageFrag(contextfrag.MessageFragInput{
		ID:        id,
		Message:   msg,
		Kind:      contextfrag.KindConversationEvent,
		Slot:      contextfrag.SlotHistory,
		Scope:     contextfrag.Scope{BotID: "bot-1"},
		Source:    "run_config_fields",
		Collector: "history_messages",
	})
}

func assistantToolCallMessage(callID, toolName, text string) sdk.Message {
	parts := []sdk.MessagePart{}
	if text != "" {
		parts = append(parts, sdk.TextPart{Text: text})
	}
	parts = append(parts, sdk.ToolCallPart{ToolCallID: callID, ToolName: toolName, Input: map[string]any{}})
	return sdk.Message{Role: sdk.MessageRoleAssistant, Content: parts}
}

func toolResultMessage(callID, toolName, value string) sdk.Message {
	return sdk.Message{Role: sdk.MessageRoleTool, Content: []sdk.MessagePart{
		sdk.ToolResultPart{ToolCallID: callID, ToolName: toolName, Result: value},
	}}
}

func toolExchangeFixture() []contextfrag.ContextFrag {
	return []contextfrag.ContextFrag{
		historyMessageFrag("h0", sdk.UserMessage("question")),
		historyMessageFrag("h1", assistantToolCallMessage("call-1", "web_search", "let me look")),
		historyMessageFrag("h2", toolResultMessage("call-1", "web_search", "bulky result")),
		historyMessageFrag("h3", assistantToolCallMessage("ask-1", userinput.ToolNameAskUser, "")),
		historyMessageFrag("h4", toolResultMessage("ask-1", userinput.ToolNameAskUser, "user picked B")),
		historyMessageFrag("h5", sdk.AssistantMessage("final answer")),
	}
}

func TestToolExchangePolicyStripsBulkyExchangesKeepsAskUser(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	result := selector.Select(toolExchangeFixture(), selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{
		ToolExchange: &contextfrag.ToolExchangePolicy{},
	})

	ids := make(map[string]bool, len(result.Selected))
	for _, frag := range result.Selected {
		ids[frag.ID] = true
	}
	if ids["h2"] {
		t.Fatal("bulky tool result must be stripped")
	}
	if !ids["h3"] || !ids["h4"] {
		t.Fatal("ask_user call and result must survive")
	}
	if !ids["h0"] || !ids["h5"] {
		t.Fatal("plain conversation turns must survive")
	}
	// h1 keeps its visible text but loses the non-ask_user tool call part.
	for _, frag := range result.Selected {
		if frag.ID != "h1" {
			continue
		}
		msg := discussFragMessage(frag)
		if msg == nil {
			t.Fatal("h1 must remain a message frag")
		}
		for _, part := range msg.Content {
			if call, ok := part.(sdk.ToolCallPart); ok && !strings.EqualFold(call.ToolName, userinput.ToolNameAskUser) {
				t.Fatalf("non-ask_user tool call survived: %#v", call)
			}
		}
	}
	if len(result.Edited) == 0 {
		t.Fatal("stripping tool calls from kept frags must record an edit trace")
	}
	var strippedDrop bool
	for _, record := range result.Summary.DropReasons {
		if record.FragID == "h2" && strings.Contains(record.Reason, "tool_exchange") {
			strippedDrop = true
		}
	}
	if !strippedDrop {
		t.Fatalf("stripped tool result must carry a tool_exchange drop reason: %+v", result.Summary.DropReasons)
	}
}

func TestToolExchangePolicyThresholdGate(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	result := selector.Select(toolExchangeFixture(), selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{
		ToolExchange: &contextfrag.ToolExchangePolicy{MinMessages: 10},
	})
	if len(result.Selected) != 6 {
		t.Fatalf("below the threshold nothing is stripped, selected = %d", len(result.Selected))
	}
}

func TestToolExchangePolicyNilKeepsEverything(t *testing.T) {
	t.Parallel()

	selector := &FragmentSelector{}
	result := selector.Select(toolExchangeFixture(), selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{})
	if len(result.Selected) != 6 {
		t.Fatalf("no policy means no stripping, selected = %d", len(result.Selected))
	}
}
