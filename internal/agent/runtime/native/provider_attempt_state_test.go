package native

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"
)

func providerAttemptText(msg sdk.Message) string {
	if len(msg.Content) == 0 {
		return ""
	}
	part, _ := msg.Content[0].(sdk.TextPart)
	return part.Text
}

func TestProviderAttemptStateBuildsRawRetryMessages(t *testing.T) {
	t.Parallel()

	cacheControl := &sdk.CacheControl{Type: "ephemeral"}
	const exactLargeInteger = int64(9007199254740993)
	toolCall := sdk.Message{
		Role: sdk.MessageRoleAssistant,
		Content: []sdk.MessagePart{sdk.ToolCallPart{
			ToolCallID: "call-1",
			ToolName:   "lookup",
			Input:      map[string]any{"id": exactLargeInteger},
		}},
	}
	toolResult := sdk.Message{
		Role: sdk.MessageRoleTool,
		Content: []sdk.MessagePart{sdk.ToolResultPart{
			ToolCallID: "call-1",
			ToolName:   "lookup",
			Result:     map[string]any{"id": exactLargeInteger},
		}},
	}
	state := &providerAttemptState{}
	state.store(&sdk.GenerateParams{Messages: []sdk.Message{
		{
			Role: sdk.MessageRoleSystem,
			Content: []sdk.MessagePart{sdk.TextPart{
				Text:         "stable system",
				CacheControl: cacheControl,
			}},
		},
		{
			Role: sdk.MessageRoleUser,
			Content: []sdk.MessagePart{sdk.TextPart{
				Text:             "task",
				CacheControl:     cacheControl,
				ProviderMetadata: map[string]any{"trace": "keep"},
			}},
		},
		toolCall,
		toolResult,
		sdk.UserMessage("dynamic hook"),
	}}, 1, true)
	previous := &sdk.StreamResult{Steps: []sdk.StepResult{
		{Messages: []sdk.Message{toolCall, toolResult}},
		{Messages: []sdk.Message{sdk.AssistantMessage("partial retry tail")}},
	}}

	messages, ok := state.retryMessages(previous)
	if !ok || len(messages) != 5 {
		t.Fatalf("retry messages = %#v, %v; want raw input plus current tail", messages, ok)
	}
	if messages[0].Role != sdk.MessageRoleUser || providerAttemptText(messages[0]) != "task" {
		t.Fatalf("promoted system was not removed: %#v", messages)
	}
	taskPart, ok := messages[0].Content[0].(sdk.TextPart)
	if !ok || taskPart.CacheControl != nil || taskPart.ProviderMetadata["trace"] != "keep" {
		t.Fatalf("raw task part = %#v, want cache control cleared and provider metadata retained", messages[0].Content)
	}
	retryCall, ok := messages[1].Content[0].(sdk.ToolCallPart)
	if !ok {
		t.Fatalf("retry tool call = %#v, want sdk.ToolCallPart", messages[1].Content)
	}
	callID, ok := retryCall.Input.(map[string]any)["id"].(int64)
	if !ok || callID != exactLargeInteger {
		t.Fatalf("retry tool input id = %#v, want exact int64 %d", retryCall.Input, exactLargeInteger)
	}
	retryResult, ok := messages[2].Content[0].(sdk.ToolResultPart)
	if !ok {
		t.Fatalf("retry tool result = %#v, want sdk.ToolResultPart", messages[2].Content)
	}
	resultID, ok := retryResult.Result.(map[string]any)["id"].(int64)
	if !ok || resultID != exactLargeInteger {
		t.Fatalf("retry tool result id = %#v, want exact int64 %d", retryResult.Result, exactLargeInteger)
	}
	if providerAttemptText(messages[len(messages)-1]) != "partial retry tail" {
		t.Fatalf("current partial output was not appended: %#v", messages)
	}
}
