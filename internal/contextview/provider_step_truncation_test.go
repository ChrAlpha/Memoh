package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
)

func loopSpanWithToolCycles(prefix []sdk.Message, cycles int, resultSize int) []sdk.Message {
	messages := append([]sdk.Message(nil), prefix...)
	for i := 0; i < cycles; i++ {
		callID := "call-" + string(rune('a'+i))
		messages = append(messages,
			assistantToolCallMessage(callID, "lookup", ""),
			toolResultMessage(callID, "lookup", strings.Repeat("x", resultSize)),
		)
	}
	return messages
}

func TestStepReselectionTruncatesOldToolResultsKeepsRecent(t *testing.T) {
	t.Parallel()

	prefix := []sdk.Message{sdk.UserMessage("task")}
	messages := loopSpanWithToolCycles(prefix, 3, 1000)

	selection := SelectProviderStepMessages(context.Background(), agentpkg.ContextStepSelectionInput{
		Scope:                 contextfrag.Scope{BotID: "bot-1"},
		InitialMessageCount:   len(prefix),
		Messages:              messages,
		KeepRecentToolResults: 1,
	})
	if selection.Messages == nil {
		t.Fatal("truncation must produce a message override")
	}
	if selection.Truncated != 2 {
		t.Fatalf("truncated = %d, want the two older results", selection.Truncated)
	}

	var toolResults []sdk.ToolResultPart
	for _, msg := range selection.Messages {
		if msg.Role != sdk.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if result, ok := part.(sdk.ToolResultPart); ok {
				toolResults = append(toolResults, result)
			}
		}
	}
	if len(toolResults) != 3 {
		t.Fatalf("tool results = %d, want all three preserved as parts", len(toolResults))
	}
	for i, result := range toolResults[:2] {
		text, _ := result.Result.(string)
		if !strings.Contains(text, "pruned") {
			t.Fatalf("older result %d must be truncated, got %q", i, text)
		}
	}
	newest, _ := toolResults[2].Result.(string)
	if strings.Contains(newest, "pruned") {
		t.Fatalf("newest cycle must stay intact, got %q", newest)
	}
}

func TestStepReselectionSkipsTruncationForSmallResults(t *testing.T) {
	t.Parallel()

	prefix := []sdk.Message{sdk.UserMessage("task")}
	messages := loopSpanWithToolCycles(prefix, 3, 40)

	selection := SelectProviderStepMessages(context.Background(), agentpkg.ContextStepSelectionInput{
		Scope:                 contextfrag.Scope{BotID: "bot-1"},
		InitialMessageCount:   len(prefix),
		Messages:              messages,
		KeepRecentToolResults: 1,
	})
	if selection.Truncated != 0 {
		t.Fatalf("small results must not be truncated, got %d", selection.Truncated)
	}
}

func TestStepReselectionTruncationRespectsMinMessages(t *testing.T) {
	t.Parallel()

	prefix := []sdk.Message{sdk.UserMessage("task")}
	messages := loopSpanWithToolCycles(prefix, 3, 1000)

	selection := SelectProviderStepMessages(context.Background(), agentpkg.ContextStepSelectionInput{
		Scope:                 contextfrag.Scope{BotID: "bot-1"},
		InitialMessageCount:   len(prefix),
		Messages:              messages,
		KeepRecentToolResults: 1,
		MinMessages:           50,
	})
	if selection.Truncated != 0 {
		t.Fatalf("below the message threshold nothing truncates, got %d", selection.Truncated)
	}
}

func TestStepReselectionBudgetDropsKeepToolClosuresAtomic(t *testing.T) {
	t.Parallel()

	prefix := []sdk.Message{sdk.UserMessage("task")}
	messages := loopSpanWithToolCycles(prefix, 4, 800)

	selection := SelectProviderStepMessages(context.Background(), agentpkg.ContextStepSelectionInput{
		Scope:               contextfrag.Scope{BotID: "bot-1"},
		InitialMessageCount: len(prefix),
		Messages:            messages,
		// Leave room for the protected newest closure and trim notice while
		// forcing older closures out as atomic units.
		BudgetMaxTokens: 400,
	})
	if selection.Messages == nil || selection.Dropped == 0 {
		t.Fatalf("budget pressure must drop loop span content: %+v", selection)
	}

	calls := map[string]bool{}
	for _, msg := range selection.Messages {
		for _, part := range msg.Content {
			if call, ok := part.(sdk.ToolCallPart); ok {
				calls[call.ToolCallID] = true
			}
		}
	}
	for _, msg := range selection.Messages {
		for _, part := range msg.Content {
			if result, ok := part.(sdk.ToolResultPart); ok && !calls[result.ToolCallID] {
				t.Fatalf("orphan tool result survived budget drop: %s", result.ToolCallID)
			}
		}
	}
	for _, msg := range selection.Messages {
		if msg.Role != sdk.MessageRoleAssistant {
			continue
		}
		for _, part := range msg.Content {
			call, ok := part.(sdk.ToolCallPart)
			if !ok {
				continue
			}
			answered := false
			for _, other := range selection.Messages {
				for _, otherPart := range other.Content {
					if result, ok := otherPart.(sdk.ToolResultPart); ok && result.ToolCallID == call.ToolCallID {
						answered = true
					}
				}
			}
			if !answered {
				t.Fatalf("orphan tool call survived budget drop: %s", call.ToolCallID)
			}
		}
	}
	if reasons := selection.DropReasons; reasons["preserve_tool_closure"] > 0 {
		t.Fatalf("budget drops must report their droppable cause, not the closure tag: %#v", reasons)
	}
}
