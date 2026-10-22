package agenttest

import (
	"encoding/json"
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// TerminalEvent is the agent_end event of a run whose answer is assistantText.
func TerminalEvent(t *testing.T, assistantText string) native.StreamEvent {
	t.Helper()
	messagesJSON, err := json.Marshal([]sdk.Message{sdk.AssistantMessage(assistantText)})
	if err != nil {
		t.Fatalf("marshal terminal messages: %v", err)
	}
	usageJSON, err := json.Marshal(sdk.Usage{InputTokens: 12, OutputTokens: 3})
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	return native.StreamEvent{Type: native.EventAgentEnd, Messages: messagesJSON, Usage: usageJSON}
}

// TriggerRequest is the request of a scheduled run.
func TriggerRequest() runreq.ChatRequest {
	return runreq.ChatRequest{BotID: "bot-1", ChatID: "bot-1", ThreadID: "session-1", Query: "run scheduled task"}
}
