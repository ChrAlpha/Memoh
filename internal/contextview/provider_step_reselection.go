package contextview

import (
	"context"
	"fmt"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
)

func SelectProviderStepMessages(ctx context.Context, input agentpkg.ContextStepSelectionInput) agentpkg.ContextStepSelectionResult {
	if input.InitialMessageCount < 0 || input.InitialMessageCount >= len(input.Messages) {
		return agentpkg.ContextStepSelectionResult{}
	}
	loopMessages := input.Messages[input.InitialMessageCount:]
	if len(loopMessages) == 0 {
		return agentpkg.ContextStepSelectionResult{}
	}

	frags, err := (&HistoryMessagesCollector{}).Collect(ctx, CollectRequest{
		Scope:  input.Scope,
		Intent: contextfrag.IntentRunConfigPreProvider,
		Config: HistoryMessagesConfig{
			Messages:        loopMessages,
			TrimmablePrefix: len(loopMessages),
		},
	})
	if err != nil || len(frags) == 0 {
		return agentpkg.ContextStepSelectionResult{}
	}

	selector := &FragmentSelector{}
	selection := selector.Select(frags, selector.ProfileFor(contextfrag.IntentRunConfigPreProvider), BudgetEnvelope{
		MaxTokens: input.BudgetMaxTokens,
	})

	selected := selectedProviderStepFrags(selection, input.Scope)
	truncated := 0
	if len(input.Messages) >= input.MinMessages {
		selected, truncated = truncateOldToolResultFrags(selected, input.KeepRecentToolResults)
	}
	if len(selection.Dropped) == 0 && truncated == 0 {
		return agentpkg.ContextStepSelectionResult{}
	}

	messages := make([]sdk.Message, 0, input.InitialMessageCount+len(selected))
	messages = append(messages, cloneSDKMessages(input.Messages[:input.InitialMessageCount])...)
	messages = append(messages, sdkMessagesFromFrags(selected)...)

	return agentpkg.ContextStepSelectionResult{
		Messages:    messages,
		Dropped:     len(selection.Dropped),
		Truncated:   truncated,
		DropReasons: dropReasonHistogram(selection.Summary.DropReasons),
	}
}

const stepToolResultTruncateBytes = 512

// truncateOldToolResultFrags keeps the most recent keepRecent complete tool
// cycles intact and replaces older bulky tool results with a size summary,
// preserving the ToolResultPart shape so provider serializers stay happy.
// keepRecent <= 0 disables truncation.
func truncateOldToolResultFrags(frags []contextfrag.ContextFrag, keepRecent int) ([]contextfrag.ContextFrag, int) {
	if keepRecent <= 0 {
		return frags, 0
	}
	recentCycles := 0
	cutoff := -1
	for i := len(frags) - 1; i >= 0; i-- {
		msg := discussFragMessage(frags[i])
		if msg == nil || msg.Role != sdk.MessageRoleTool {
			continue
		}
		recentCycles++
		if recentCycles > keepRecent {
			cutoff = i
			break
		}
	}
	if cutoff < 0 {
		return frags, 0
	}
	truncated := 0
	out := make([]contextfrag.ContextFrag, len(frags))
	copy(out, frags)
	for i := 0; i <= cutoff; i++ {
		msg := discussFragMessage(out[i])
		if msg == nil || msg.Role != sdk.MessageRoleTool {
			continue
		}
		replaced, changed := truncateToolResultMessage(*msg)
		if !changed {
			continue
		}
		out[i] = rebuildMessageFrag(out[i], replaced)
		truncated++
	}
	return out, truncated
}

func truncateToolResultMessage(msg sdk.Message) (sdk.Message, bool) {
	contentSize := 0
	for _, part := range msg.Content {
		if result, ok := part.(sdk.ToolResultPart); ok {
			contentSize += len(fmt.Sprintf("%v", result.Result))
		}
	}
	if contentSize <= stepToolResultTruncateBytes {
		return msg, false
	}
	parts := make([]sdk.MessagePart, 0, len(msg.Content))
	for _, part := range msg.Content {
		if result, ok := part.(sdk.ToolResultPart); ok {
			parts = append(parts, sdk.ToolResultPart{
				ToolCallID: result.ToolCallID,
				ToolName:   result.ToolName,
				Result:     fmt.Sprintf("[tool result pruned: %d bytes]", contentSize),
			})
			continue
		}
		parts = append(parts, part)
	}
	return sdk.Message{Role: msg.Role, Content: parts}, true
}

func selectedProviderStepFrags(selection SelectionResult, scope contextfrag.Scope) []contextfrag.ContextFrag {
	if !selection.TrimNotice || selection.TrimNoticeIndex < 0 || selection.TrimNoticeIndex > len(selection.Selected) {
		return selection.Selected
	}
	notice := contextfrag.NormalizeContextRefs([]contextfrag.ContextFrag{TrimNoticeFrag(scope)})[0]
	selected := make([]contextfrag.ContextFrag, 0, len(selection.Selected)+1)
	selected = append(selected, selection.Selected[:selection.TrimNoticeIndex]...)
	selected = append(selected, notice)
	selected = append(selected, selection.Selected[selection.TrimNoticeIndex:]...)
	return selected
}

func sdkMessagesFromFrags(frags []contextfrag.ContextFrag) []sdk.Message {
	var messages []sdk.Message
	for _, frag := range frags {
		for _, part := range frag.Parts {
			if part.Type != contextfrag.PartSDKMessage {
				continue
			}
			if msg := sdkMessagePart(part); msg != nil {
				messages = append(messages, cloneSDKMessage(*msg))
			}
		}
	}
	return messages
}

func cloneSDKMessages(messages []sdk.Message) []sdk.Message {
	out := make([]sdk.Message, len(messages))
	for i, msg := range messages {
		out[i] = cloneSDKMessage(msg)
	}
	return out
}

func dropReasonHistogram(records []DropRecord) map[string]int {
	if len(records) == 0 {
		return nil
	}
	out := make(map[string]int, len(records))
	for _, record := range records {
		out[record.Reason]++
	}
	return out
}
