package flow

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/conversation"
)

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
