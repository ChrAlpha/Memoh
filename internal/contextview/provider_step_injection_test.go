package contextview

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func appendToolCycles(messages []sdk.Message, ids []string, resultSize int) []sdk.Message {
	for _, id := range ids {
		messages = append(messages,
			assistantToolCallMessage(id, "lookup", ""),
			toolResultMessage(id, "lookup", strings.Repeat("x", resultSize)),
		)
	}
	return messages
}

func selectionHasUserText(messages []sdk.Message, text string) bool {
	for _, msg := range messages {
		if msg.Role != sdk.MessageRoleUser {
			continue
		}
		for _, part := range msg.Content {
			if tp, ok := part.(sdk.TextPart); ok && strings.Contains(tp.Text, text) {
				return true
			}
		}
	}
	return false
}

func TestMarkInjectedLoopUserFragsTypesAndProtectsOnlyUserMessages(t *testing.T) {
	t.Parallel()

	messages := appendToolCycles(nil, []string{"call-a"}, 40)
	messages = append(messages, sdk.UserMessage("<message sender=\"alice\">injected</message>"))

	frags, err := (&HistoryMessagesCollector{}).Collect(context.Background(), CollectRequest{
		Scope:  contextfrag.Scope{BotID: "bot-1"},
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: HistoryMessagesConfig{Messages: messages, TrimmablePrefix: len(messages)},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	before := make([]contextfrag.ContextFrag, len(frags))
	copy(before, frags)

	marked := markInjectedLoopUserFrags(frags)
	for i, frag := range marked {
		msg := discussFragMessage(frag)
		if msg != nil && msg.Role == sdk.MessageRoleUser {
			if frag.Kind != contextfrag.KindInjectedMessage {
				t.Fatalf("injected frag kind = %q, want %q", frag.Kind, contextfrag.KindInjectedMessage)
			}
			if frag.Budget.Overflow != contextfrag.OverflowKeep {
				t.Fatalf("injected frag overflow = %q, want keep", frag.Budget.Overflow)
			}
			continue
		}
		if frag.Kind != before[i].Kind || frag.Budget != before[i].Budget {
			t.Fatalf("non-user frag %d changed: %+v -> %+v", i, before[i], frag)
		}
	}
}

func TestStepReselectionKeepsInjectedUserMessagesUnderBudgetPressure(t *testing.T) {
	t.Parallel()

	prefix := []sdk.Message{sdk.UserMessage("task")}
	messages := appendToolCycles(prefix, []string{"call-a", "call-b"}, 800)
	messages = append(messages, sdk.UserMessage("<message sender=\"alice\" t=\"now\" channel=\"telegram\">\nfirst injected instruction\n</message>"))
	messages = appendToolCycles(messages, []string{"call-c", "call-d"}, 800)
	messages = append(messages, sdk.UserMessage("<message sender=\"alice\" t=\"later\" channel=\"telegram\">\nsecond injected instruction\n</message>"))

	selection := SelectProviderStepMessages(context.Background(), agentpkg.ContextStepSelectionInput{
		Scope:               contextfrag.Scope{BotID: "bot-1"},
		InitialMessageCount: len(prefix),
		Messages:            messages,
		BudgetMaxTokens:     200,
	})
	if selection.Messages == nil || selection.Dropped == 0 {
		t.Fatalf("budget pressure must drop loop span content: %+v", selection)
	}
	if !selectionHasUserText(selection.Messages, "first injected instruction") {
		t.Fatal("older injected user message must survive step budget pressure")
	}
	if !selectionHasUserText(selection.Messages, "second injected instruction") {
		t.Fatal("newest injected user message must survive step budget pressure")
	}
}
