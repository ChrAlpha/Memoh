package flow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/userinput"
)

func intPtr(v int) *int { return &v }

// budgetTrimViaContextView runs history messages through the provider context
// view with the given token budget, mirroring how resolve() hands trimmable
// history plus per-message estimates to the selection engine.
func budgetTrimViaContextView(t *testing.T, history []conversation.ModelMessage, budget int) []sdk.Message {
	t.Helper()
	estimates := make([]int, len(history))
	for i := range history {
		estimates[i] = estimateMessageTokens(history[i])
	}
	cfg := agentpkg.RunConfig{
		Messages:                     modelMessagesToSDKMessages(history),
		ContextHistoryTokenEstimates: estimates,
		ContextTrimmableMessages:     len(history),
		ContextBudgetMaxTokens:       budget,
		ContextScope:                 contextfrag.Scope{BotID: "bot-1", SessionID: "s1"},
	}
	got := contextview.ApplyProviderRunConfig(context.Background(), nil, cfg)
	return got.Messages
}

func rolesOf(messages []sdk.Message) []string {
	roles := make([]string, len(messages))
	for i, msg := range messages {
		roles[i] = string(msg.Role)
	}
	return roles
}

func TestBudgetTrim_DropsLeadingOrphanTool(t *testing.T) {
	t.Parallel()

	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("1111")},
		{
			Role: "assistant",
			ToolCalls: []conversation.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: conversation.ToolCallFunction{
					Name:      "calc",
					Arguments: `{"x":1}`,
				},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Content: conversation.NewTextContent("2")},
		{Role: "assistant", Content: conversation.NewTextContent("done")},
	}

	// Character-based estimates keep everything within budget=70; the point is
	// that whatever survives must never start with an orphaned tool result.
	trimmed := budgetTrimViaContextView(t, history, 70)
	if len(trimmed) == 0 {
		t.Fatal("expected non-empty trimmed messages")
	}
	if trimmed[0].Role == sdk.MessageRoleTool {
		t.Fatal("expected first trimmed message not to be tool")
	}
}

func TestBudgetTrim_CutoffAdvancesPastPairedTool(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 400)
	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent(long)},
		{
			Role: "assistant",
			ToolCalls: []conversation.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: conversation.ToolCallFunction{
					Name:      "calc",
					Arguments: `{"x":1}`,
				},
			}},
		},
		{Role: "tool", ToolCallID: "call-1", Content: conversation.NewTextContent("2")},
		{Role: "assistant", Content: conversation.NewTextContent(long)},
		{Role: "user", Content: conversation.NewTextContent("next question")},
	}

	// Budget forces the cutoff into the tool exchange: the cut must extend past
	// the tool result so no orphan survives at the head of kept history.
	trimmed := budgetTrimViaContextView(t, history, 100)
	if len(trimmed) != 3 {
		t.Fatalf("messages = %v, want [notice assistant user]", rolesOf(trimmed))
	}
	if trimmed[0].Role != sdk.MessageRoleSystem {
		t.Fatalf("first message role = %q, want trim notice", trimmed[0].Role)
	}
	for _, msg := range trimmed {
		if msg.Role == sdk.MessageRoleTool {
			t.Fatalf("orphan tool message survived: %v", rolesOf(trimmed))
		}
	}
}

func TestBudgetTrim_UnderBudgetKeepsAll(t *testing.T) {
	t.Parallel()

	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{Role: "assistant", Content: conversation.NewTextContent("hi")},
	}

	trimmed := budgetTrimViaContextView(t, history, 10)
	if len(trimmed) != 2 {
		t.Fatalf("messages under budget should all be kept, got %d", len(trimmed))
	}
}

func TestBudgetTrim_ZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("y", 4000)
	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent(long)},
		{Role: "assistant", Content: conversation.NewTextContent(long)},
	}

	trimmed := budgetTrimViaContextView(t, history, 0)
	if len(trimmed) != 2 {
		t.Fatalf("budget=0 should keep all messages, got %d", len(trimmed))
	}
}

func TestBudgetTrim_SmallBudgetTrimsButProtectsLatestUserTurn(t *testing.T) {
	t.Parallel()

	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("old message")},
		{Role: "assistant", Content: conversation.NewTextContent("old reply")},
		{Role: "user", Content: conversation.NewTextContent("new message")},
		{Role: "assistant", Content: conversation.NewTextContent("new reply")},
	}

	// Unlike the legacy trim, the selection engine never drops the latest user
	// turn: budget pressure removes older history and inserts the notice.
	trimmed := budgetTrimViaContextView(t, history, 1)
	if got := rolesOf(trimmed); len(got) != 3 ||
		trimmed[0].Role != sdk.MessageRoleSystem ||
		trimmed[1].Role != sdk.MessageRoleUser ||
		trimmed[2].Role != sdk.MessageRoleAssistant {
		t.Fatalf("messages = %v, want [notice user assistant]", got)
	}
}

func TestBudgetTrim_NoticeContentMatchesLegacyWording(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("z", 400)
	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent(long)},
		{Role: "assistant", Content: conversation.NewTextContent("ok")},
		{Role: "user", Content: conversation.NewTextContent("next")},
	}

	trimmed := budgetTrimViaContextView(t, history, 50)
	if len(trimmed) != 3 || trimmed[0].Role != sdk.MessageRoleSystem {
		t.Fatalf("messages = %v, want notice first", rolesOf(trimmed))
	}
	text, ok := trimmed[0].Content[0].(sdk.TextPart)
	if !ok || text.Text != contextview.HistoryTrimNotice {
		t.Fatalf("notice = %#v, want legacy trim notice wording", trimmed[0].Content)
	}
}

func TestBudgetTrim_PinnedTailNeverTrimmed(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("m", 4000)
	history := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent(long)},
		{Role: "assistant", Content: conversation.NewTextContent(long)},
	}
	estimates := make([]int, len(history))
	for i := range history {
		estimates[i] = estimateMessageTokens(history[i])
	}
	messages := modelMessagesToSDKMessages(history)
	messages = append(messages, sdk.UserMessage("memory context"), sdk.UserMessage("current request"))

	cfg := agentpkg.RunConfig{
		Messages:                     messages,
		ContextHistoryTokenEstimates: estimates,
		ContextTrimmableMessages:     len(history),
		ContextBudgetMaxTokens:       1,
		ContextScope:                 contextfrag.Scope{BotID: "bot-1"},
	}
	got := contextview.ApplyProviderRunConfig(context.Background(), nil, cfg)

	if len(got.Messages) != 3 {
		t.Fatalf("messages = %v, want [notice memory request]", rolesOf(got.Messages))
	}
	last, ok := got.Messages[2].Content[0].(sdk.TextPart)
	if !ok || last.Text != "current request" {
		t.Fatalf("pinned tail lost: %#v", got.Messages[2].Content)
	}
}

func TestTrimMessagesByTokens_DropsLeadingOrphanTool(t *testing.T) {
	t.Parallel()

	messages := []messageWithUsage{
		{
			Message: conversation.ModelMessage{
				Role:    "user",
				Content: conversation.NewTextContent("1111"),
			},
		},
		{
			Message: conversation.ModelMessage{
				Role: "assistant",
				ToolCalls: []conversation.ToolCall{
					{
						ID:   "call-1",
						Type: "function",
						Function: conversation.ToolCallFunction{
							Name:      "calc",
							Arguments: `{"x":1}`,
						},
					},
				},
			},
			UsageOutputTokens: intPtr(50),
		},
		{
			Message: conversation.ModelMessage{
				Role:       "tool",
				ToolCallID: "call-1",
				Content:    conversation.NewTextContent("2222"),
			},
		},
		{
			Message: conversation.ModelMessage{
				Role:    "assistant",
				Content: conversation.NewTextContent("done"),
			},
			UsageOutputTokens: intPtr(60),
		},
	}

	// Budget 2: newest assistant and tool result fit, adding the older assistant
	// tool call exceeds the budget. The cutoff initially lands on the tool result,
	// which must be skipped to avoid an orphan tool message.
	trimmed, _ := trimMessagesByTokens(nil, messages, 2)
	if len(trimmed) != 2 {
		t.Fatalf("expected truncation notice and latest assistant, got %d messages: %+v", len(trimmed), trimmed)
	}
	if trimmed[0].Role != "system" || trimmed[1].Role != "assistant" {
		t.Fatalf("expected [system, assistant], got %+v", trimmed)
	}
	for _, msg := range trimmed {
		if msg.Role == "tool" {
			t.Fatalf("expected orphan tool to be skipped, got %+v", trimmed)
		}
	}
}

func TestTrimMessagesByTokens_KeepsToolWhenPaired(t *testing.T) {
	t.Parallel()

	messages := []messageWithUsage{
		{
			Message: conversation.ModelMessage{
				Role: "assistant",
				ToolCalls: []conversation.ToolCall{
					{
						ID:   "call-1",
						Type: "function",
						Function: conversation.ToolCallFunction{
							Name:      "calc",
							Arguments: `{"x":1}`,
						},
					},
				},
			},
			UsageOutputTokens: intPtr(10),
		},
		{
			Message: conversation.ModelMessage{
				Role:       "tool",
				ToolCallID: "call-1",
				Content:    conversation.NewTextContent("2"),
			},
		},
	}

	trimmed, _ := trimMessagesByTokens(nil, messages, 100)
	if len(trimmed) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(trimmed))
	}
	if trimmed[0].Role != "assistant" || trimmed[1].Role != "tool" {
		t.Fatalf("unexpected role order: %q -> %q", trimmed[0].Role, trimmed[1].Role)
	}
}

func TestTrimMessagesByTokens_NoUsage_KeepsAll(t *testing.T) {
	t.Parallel()

	messages := []messageWithUsage{
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("hello")}},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("hi")}},
	}

	trimmed, _ := trimMessagesByTokens(nil, messages, 10)
	if len(trimmed) != 2 {
		t.Fatalf("messages without outputTokens should all be kept, got %d", len(trimmed))
	}
}

func TestTrimMessagesByTokens_ZeroMeansNoLimit(t *testing.T) {
	t.Parallel()

	messages := []messageWithUsage{
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("hello")}, UsageOutputTokens: intPtr(10000)},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("world")}, UsageOutputTokens: intPtr(10000)},
	}

	// maxTokens = 0 means "no limit configured", should keep all messages.
	trimmed, _ := trimMessagesByTokens(nil, messages, 0)
	if len(trimmed) != 2 {
		t.Fatalf("maxTokens=0 should keep all messages, got %d", len(trimmed))
	}
}

func TestTrimMessagesByTokens_SmallBudgetTrims(t *testing.T) {
	t.Parallel()

	messages := []messageWithUsage{
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("old message")}, UsageOutputTokens: intPtr(100)},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("old reply")}, UsageOutputTokens: intPtr(200)},
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent("new message")}, UsageOutputTokens: intPtr(50)},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("new reply")}, UsageOutputTokens: intPtr(60)},
	}

	// Budget of 1: should trim aggressively, NOT return all messages.
	trimmed, _ := trimMessagesByTokens(nil, messages, 1)
	if len(trimmed) >= len(messages) {
		t.Fatalf("maxTokens=1 should trim history, but got %d messages (same as input)", len(trimmed))
	}
}

func TestTrimMessagesByTokens_EstimatesFallback(t *testing.T) {
	t.Parallel()

	// Long user message without usage data — should be estimated.
	longText := make([]byte, 400)
	for i := range longText {
		longText[i] = 'x'
	}
	messages := []messageWithUsage{
		{Message: conversation.ModelMessage{Role: "user", Content: conversation.NewTextContent(string(longText))}},
		{Message: conversation.ModelMessage{Role: "assistant", Content: conversation.NewTextContent("ok")}, UsageOutputTokens: intPtr(10)},
	}

	// Budget of 50: user message is ~100 estimated tokens (400/4), should be trimmed.
	trimmed, _ := trimMessagesByTokens(nil, messages, 50)
	// When trimming occurs, a system truncation notice is prepended.
	// So we expect: 1 system notice + 1 assistant message (kept) = 2 total.
	// The key check is that the long user message was removed.
	if len(trimmed) != 2 || trimmed[0].Role != "system" || trimmed[1].Role != "assistant" {
		t.Fatalf("expected [system notice, assistant message], got %d messages: %+v", len(trimmed), trimmed)
	}
}

func TestTrimMessagesByTokens_PreservesRequiredMessage(t *testing.T) {
	t.Parallel()

	longText := make([]byte, 400)
	for i := range longText {
		longText[i] = 'x'
	}
	messages := []messageWithUsage{
		{
			ID:       "required-user",
			Required: true,
			Message: conversation.ModelMessage{
				Role:    "user",
				Content: conversation.NewTextContent("retry this exact prompt"),
			},
		},
		{
			ID: "old-assistant",
			Message: conversation.ModelMessage{
				Role:    "assistant",
				Content: conversation.NewTextContent(string(longText)),
			},
		},
		{
			ID: "new-assistant",
			Message: conversation.ModelMessage{
				Role:    "assistant",
				Content: conversation.NewTextContent("recent reply"),
			},
		},
	}

	trimmed, _ := trimMessagesByTokens(nil, messages, 5)
	if len(trimmed) < 2 {
		t.Fatalf("expected system notice and required prompt, got %d", len(trimmed))
	}
	if trimmed[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", trimmed[0].Role)
	}
	if trimmed[1].Role != "user" || trimmed[1].TextContent() != "retry this exact prompt" {
		t.Fatalf("required message was not preserved in order: %+v", trimmed)
	}
}

func TestStripToolMessages_RemovesAssistantToolCallContentParts(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal([]map[string]any{
		{"type": "reasoning", "text": "thinking"},
		{"type": "tool-call", "toolName": "read", "toolCallId": "call-1", "input": map[string]any{"path": "/tmp/a.txt"}},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	filtered := stripToolMessages([]conversation.ModelMessage{
		{
			Role:    "assistant",
			Content: content,
		},
		{
			Role:    "assistant",
			Content: conversation.NewTextContent("保留这条消息"),
		},
	})

	if len(filtered) != 1 {
		t.Fatalf("expected 1 message after filtering, got %d", len(filtered))
	}
	if filtered[0].TextContent() != "保留这条消息" {
		t.Fatalf("unexpected remaining message: %+v", filtered[0])
	}
}

func TestStripToolMessages_PreservesAskUserInteraction(t *testing.T) {
	t.Parallel()

	callContent, err := json.Marshal([]map[string]any{
		{"type": "text", "text": "请回答这一题："},
		{
			"type":       "tool-call",
			"toolName":   userinput.ToolNameAskUser,
			"toolCallId": "ask-1",
			"input": map[string]any{
				"questions": []any{
					map[string]any{
						"text": "选哪一个？",
						"kind": "single_select",
						"options": []any{
							map[string]any{"label": "A"},
							map[string]any{"label": "B"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal call content: %v", err)
	}
	resultContent, err := json.Marshal([]map[string]any{
		{
			"type":       "tool-result",
			"toolName":   userinput.ToolNameAskUser,
			"toolCallId": "ask-1",
			"result": map[string]any{
				"status": "submitted",
				"answers": []any{
					map[string]any{
						"question": "选哪一个？",
						"selected": []any{map[string]any{"label": "B"}},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal result content: %v", err)
	}
	readContent, err := json.Marshal([]map[string]any{
		{"type": "tool-call", "toolName": "read", "toolCallId": "read-1", "input": map[string]any{"path": "/tmp/a.txt"}},
	})
	if err != nil {
		t.Fatalf("marshal read content: %v", err)
	}

	filtered := stripToolMessages([]conversation.ModelMessage{
		{Role: "assistant", Content: callContent},
		{Role: "tool", Content: resultContent},
		{Role: "assistant", Content: readContent},
		{Role: "tool", Content: conversation.NewTextContent("large output")},
	})

	if len(filtered) != 2 {
		t.Fatalf("expected ask_user call and result to remain, got %d messages: %+v", len(filtered), filtered)
	}
	if filtered[0].Role != "assistant" || filtered[1].Role != "tool" {
		t.Fatalf("unexpected roles after filtering: %+v", filtered)
	}

	var callParts []map[string]any
	if err := json.Unmarshal(filtered[0].Content, &callParts); err != nil {
		t.Fatalf("unmarshal preserved call content: %v", err)
	}
	if len(callParts) != 2 || callParts[1]["toolName"] != userinput.ToolNameAskUser {
		t.Fatalf("ask_user tool call was not preserved: %#v", callParts)
	}

	var resultParts []map[string]any
	if err := json.Unmarshal(filtered[1].Content, &resultParts); err != nil {
		t.Fatalf("unmarshal preserved result content: %v", err)
	}
	if len(resultParts) != 1 || resultParts[0]["toolName"] != userinput.ToolNameAskUser {
		t.Fatalf("ask_user tool result was not preserved: %#v", resultParts)
	}
}
